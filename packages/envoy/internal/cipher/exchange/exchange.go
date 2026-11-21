// Package exchange decides what a verified subject token entitles its holder to write in
// Dispatch: the claims a minted Dispatch access token carries — the session that is writing, the
// person it acts for or the service it is, and its list of projects, either the one its
// operator's entry names or the ones a trade acting as its person asked for, none at all for a
// token that only reads, or the one whose results a CI run writes with a token of a result
// entry — and, beside them, the principal the session name belongs to.
//
// It is the policy half of the RFC 8693 token exchange and nothing else. It verifies no token,
// reads no configuration, takes no clock and makes no call: it is handed entries someone else
// parsed and the already-verified claims of a subject token, and it answers Claims or refuses with
// a sentinel the caller maps onto an OAuth error. A Set is built once and is then immutable, so
// the same subject token decides the same way however many exchanges run at once.
//
// Five kinds of subject token reach it, and what decides each one's claims differs:
//
//   - a pod's Kubernetes service-account token, where an entry keyed on the account subject
//     (system:serviceaccount:<namespace>:<name>) gives the person and the project, or no project
//     for an entry that names none (<service account>==<person>), or a read entry keyed on it
//     gives a service and the session its token reads as, and the token must carry the pod it is
//     bound to;
//   - a continuous-integration workflow run's GitHub token, where an entry keyed on the
//     repository, the workflow's path and the ref it ran on gives the project and the person, or
//     a result entry keyed on the repository and the workflow's path, with a pattern of the refs
//     it takes, gives the project whose results the run writes, the person, and the run itself;
//   - a machine's Google identity token, where an entry keyed on the account's email address
//     gives the service, may pin the Compute Engine project and instance names the machine may
//     carry, and may give a person and a project as a pod's entry does;
//   - a person's own Google identity token, carried with the machine's as the actor token, where
//     the Workspace domain decides the person and that same machine entry decides the box they
//     may sign in from;
//   - a dev box session's machine credential, which no entry lists, because the person is the
//     approver the credential already records.
//
// The principal is the caller's standing identity, never the instance asking: the listed account
// for a pod, the listed workflow for a run, the listed workflow of a result entry, whatever its
// ref, for a run that writes results, the listed Google account for a machine, the person for a
// sign-in and for a dev box. A session name belongs to one principal, so a pod's
// replacement and a workflow's
// next run keep writing under the name
// their predecessor held, and a caller of any other principal is refused it. Instance is the
// second rule beside it, and the only thing that separates two callers of one principal: the pod
// uid a registration has to name for a pod of a free-session entry to write under a session name.
// for a pod, the listed workflow for a run, the listed Google account for a machine, the person
// for a sign-in and for a dev box. A session name belongs to one principal, so a pod's
// replacement and a workflow's next run keep writing under the name their predecessor held, and
// a caller of any other principal is refused it, but for a pod of an account the broker pairs
// with the holder's (BROKER_SESSION_ACCOUNT_PAIRS, oauthstore.Store.BindSession), whose claim
// takes the name. Instance is the second rule beside it, and the only thing that separates two
// callers of one principal: the pod uid a registration has to name for a pod of a free-session
// entry to write under a session name.
package exchange

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc"
)

// Why a subject token is refused. Each stands for one answer an operator can act on: list the
// identity (ErrNoEntry), list it once (ErrTwoEntries), mint the token in a pod rather than by hand
// (ErrNoPodBinding), ask under the session the entry fixes (ErrSessionMismatch), name a session
// (ErrSessionRequired), name a usable one (ErrSessionInvalid), present a token that carries the
// claims its kind must (ErrClaimMissing), verify the machine's or the person's address with its
// provider (ErrEmailNotVerified), sign in with an account of the configured Workspace domain
// (ErrNotInDomain), present the machine's own identity token beside the person's
// (ErrNoActorToken), mint the machine's token on a Compute Engine instance or unpin its entry
// (ErrNoComputeEngine), run the machine where its entry allows (ErrMachineNotAllowed), of a
// registration, leave an entry that fixes a session alone (ErrSessionFixed), ask for no project
// but the one an operator's entry gives (ErrProjectsFixed), or ask for no project at all for a
// token that names no person (ErrProjectsNoPerson).
var (
	ErrNoEntry           = errors.New("exchange: no entry lists this identity")
	ErrTwoEntries        = errors.New("exchange: two entries list this identity")
	ErrNoPodBinding      = errors.New("exchange: the subject token is bound to no pod")
	ErrSessionMismatch   = errors.New("exchange: the request names a session the entry does not fix")
	ErrSessionRequired   = errors.New("exchange: the request names no session and the entry fixes none")
	ErrSessionInvalid    = errors.New("exchange: the request names an unusable session")
	ErrClaimMissing      = errors.New("exchange: the subject token carries no such claim")
	ErrEmailNotVerified  = errors.New("exchange: the subject token's email address is not verified")
	ErrNotInDomain       = errors.New("exchange: the person's Google account is not in the sign-in domain")
	ErrNoActorToken      = errors.New("exchange: the person's token came with no machine acting for them")
	ErrNoComputeEngine   = errors.New("exchange: the subject token carries no Compute Engine claims")
	ErrMachineNotAllowed = errors.New("exchange: the machine is not one the entry allows")
	ErrSessionFixed      = errors.New("exchange: the entry fixes the session name its tokens carry")
	ErrProjectsFixed     = errors.New("exchange: the request names projects other than the one its entry gives")
	ErrProjectsNoPerson  = errors.New("exchange: a token that names no person names no project")
)

// MaxSessionBytes is the longest session name a request may carry. A session name is written into
// every record the token's writes produce and read back in a dashboard, so it is bounded; 200
// bytes is past any name a Legion session or a person's tool has reason to use.
const MaxSessionBytes = 200

// accountPattern is a Kubernetes service account's subject, the one a pod's projected token
// carries: system:serviceaccount:<namespace>:<name>, the namespace a DNS label and the name a DNS
// subdomain. It is the grammar the broker's configuration states for the same accounts, repeated
// here because this package reads no configuration and so cannot import the one that parses it.
var accountPattern = regexp.MustCompile(`^system:serviceaccount:[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?:[a-z0-9]([-.a-z0-9]{0,251}[a-z0-9])?$`)

// repositoryPattern is a GitHub repository as its token's `repository` claim names it,
// <owner>/<name>: the owner a letter or digit, then up to 38 letters, digits, hyphens and
// underscores, since a managed user's login ends in `_<shortcode>` (`mona-cat_octo`); the name up
// to 100 of letters, digits, hyphens, underscores and dots.
var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// googleAccountPattern is a Google service account's email address as a Google identity token
// carries it in its `email` claim: lowercase, since that is the one spelling a token holds. It is
// the grammar the broker's configuration states for the same accounts, repeated here for the
// reason accountPattern is.
var googleAccountPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._%+-]*@[a-z0-9][a-z0-9.-]*\.[a-z]{2,}$`)

// ProjectScope says what a minted token's list of projects means. Its zero value is no scope at
// all, so a Claims that was not built by this package carries one that every reader refuses:
// the absence of a project must be a decision this package made, never a field nobody filled in.
type ProjectScope int

const (
	// ProjectScopeUnset is the zero value, which no decision answers. A project check handed it
	// refuses the token: a Claims built by anything but this package's constructors has decided
	// nothing, and 'no project named' must not read as 'no oversight'.
	ProjectScopeUnset ProjectScope = iota
	// ProjectScopeNamed is a token an operator's entry mapped: it acts for the entry's person,
	// and Claims.Projects holds the entry's one project, or none for an account entry naming none.
	ProjectScopeNamed
	// ProjectScopePerson is a token that acts as its person: Claims.Projects holds the projects
	// its trade asked for, none or more, which the exchange holds to what Dispatch says that
	// person may see before it mints (internal/cipher/oauth). It is what a person's own
	// credential gets, and what a machine whose entry names no person gets, with no project.
	ProjectScopePerson
	// ProjectScopeNone is a token that may write no project and names none: it holds the OAuth
	// scope oauthwire.ScopeRead and nothing else, so it reads only the routes a resource server
	// lists for that scope, and only on a resource server that serves it. It is what a read
	// entry's pod gets, and no other decision answers it. Its String differs from
	// ProjectScopeUnset's, because 'writes nothing' is a decision this package made and 'never
	// decided' is not one.
	ProjectScopeNone
	// ProjectScopeResults is a token that may write the results of the one project
	// Claims.Projects names, for the run Claims.CI carries, and nothing else: it holds the OAuth
	// scope oauthwire.ScopeResults, so it reaches only the routes a resource server lists for
	// that scope, and only on a resource server that serves it. It is what a run of a result
	// entry's workflow gets, and no other decision answers it.
	ProjectScopeResults
)

// ComputeEngine is the instance a Google identity token was minted on, the `google` →
// `compute_engine` claims of a token asked for with format=full. Every field is empty for a token
// minted anywhere else: one a workload identity pool minted, and the one a pod's metadata server
// on Google Kubernetes Engine answers, neither of which runs on an instance of the caller's.
type ComputeEngine struct {
	// Project is the Google Cloud project the instance runs in (project_id).
	Project string
	// Zone is the zone it runs in.
	Zone string
	// Instance is the instance's name (instance_name), the one an entry's pattern matches.
	Instance string
	// InstanceID is the instance's numeric id, unique within a project and a zone.
	InstanceID string
}

// Whole reports whether the token carried a complete Compute Engine identity. A token carries all
// four of these claims or none, so anything between the two is a token this package will not read
// an identity out of.
func (c ComputeEngine) Whole() bool {
	return c.Project != "" && c.Zone != "" && c.Instance != "" && c.InstanceID != ""
}

// Empty reports whether the token carried no Compute Engine claim at all, which is what a token
// minted through a workload identity pool and a pod's own metadata token carry.
func (c ComputeEngine) Empty() bool {
	return c == ComputeEngine{}
}

// GoogleIdentity is what a Google identity token says about the machine presenting it: the
// account it was minted for, whether the provider holds that address verified, and the Compute
// Engine instance it was minted on, when it was minted on one.
type GoogleIdentity struct {
	// Account is the `email` claim, the service account the token was minted for.
	Account string
	// EmailVerified is the `email_verified` claim. Google writes it as a JSON boolean and some
	// of its endpoints as the string "true", so both are read.
	EmailVerified bool
	// Compute is the instance the token was minted on, every field empty for a token minted off
	// one.
	Compute ComputeEngine
}

// ReadGoogleIdentity reads a Google identity token's own claims out of the claim map of a token
// something else has already verified. It is the one reading of those claim shapes: this package
// decides on them, the broker's own Google token verifier reads the same token
// (api.GoogleTokens.VerifyGoogle), and
// a caller authenticated by such a token reads the account off it, so all three read one
// declaration rather than three spellings of Google's claim names. It refuses nothing: a claim
// that is absent or of another JSON type reads as empty, and which of those is a refusal is the
// reader's own rule.
func ReadGoogleIdentity(token map[string]any) GoogleIdentity {
	identity := GoogleIdentity{EmailVerified: isVerified(token["email_verified"])}
	identity.Account, _ = token["email"].(string)
	google, _ := token["google"].(map[string]any)
	compute, _ := google["compute_engine"].(map[string]any)
	identity.Compute.Project, _ = compute["project_id"].(string)
	identity.Compute.Zone, _ = compute["zone"].(string)
	identity.Compute.Instance, _ = compute["instance_name"].(string)
	identity.Compute.InstanceID, _ = compute["instance_id"].(string)
	return identity
}

// isVerified reads an `email_verified` claim, which is a JSON boolean on a token from Google's
// metadata server and the string "true" from some of its other endpoints.
func isVerified(claim any) bool {
	switch value := claim.(type) {
	case bool:
		return value
	case string:
		return value == "true"
	}
	return false
}

// MachineSubject names one machine as the broker keys the launcher credential its exchange
// mints: the row's `subject`, under the live-unique index that makes each machine's credential
// its own and lets its next exchange move that one credential rather than add another.
//
// It is <machine>#<thumbprint>: the machine, and the RFC 7638 thumbprint of the key the
// request's DPoP proof presented. The key is part of the name because the credential is bound
// to that key and the subject token is a bearer: a copy of a machine's Google identity token
// presented with a key of the copier's own would otherwise move the machine's one credential
// onto that key, and whoever holds a machine's credential ends that machine's sessions
// (enroll.Service.Revoke). Keyed by the key, such a copy mints a credential of its own and
// moves nothing of the machine's.
//
// The machine is <account>@<project>/<zone>/<instance id> for a token minted on a Compute
// Engine instance, and the account alone for a token carrying no Compute Engine claim at all —
// one minted through a workload identity pool, and the one a pod's metadata server on Google
// Kubernetes Engine answers. An instance id is unique only within a project and a zone, so
// neither the id nor the account alone names one machine; the whole of it does. Naming the
// instance still earns its place now that the key tells two machines apart: two instances of
// one account presenting one key — a key baked into an image — ask for two subjects with one
// key, which launcher_credentials_live_key refuses by name, where under one subject they would
// quietly share a credential and each could end the other's sessions.
//
// What the key in the name costs is that a machine making a new key leaves its old credential
// behind until that credential's own expiry, which is the subject token's, and the sessions
// enrolled under it stop renewing when it lapses (enroll.Service.Renew). So a machine keeps its
// key rather than making one per process: `agent-secrets-helper` reads it back from
// ~/.config/agent-secrets/identity-key.pem, mode 0600, and makes one only at the machine's
// first trade (helper.LoadOrMakeIdentityKey). A helper that lost that file, or a machine that
// is rebuilt, trades a new subject and leaves the old credential to lapse, and the sessions it
// enrolled are gone with the processes that held them.
func MachineSubject(identity GoogleIdentity, thumbprint string) string {
	name := identity.Account
	if identity.Compute.Whole() {
		name += "@" + identity.Compute.Project + "/" + identity.Compute.Zone + "/" + identity.Compute.InstanceID
	}
	return name + "#" + thumbprint
}

// signInSubjectPrefix opens the subject of a person's sign-in on a machine, so that no subject
// of any other kind can spell one: a Google account never begins with it, and a Kubernetes
// ServiceAccount's subject begins with "system:serviceaccount:".
const signInSubjectPrefix = "signin:"

// PersonSubject names one person's sign-in on one box as the broker keys the launcher credential
// the exchange mints for it: the row's `subject`, under the live-unique index that makes each
// sign-in's credential its own and lets the sign-in's next trade move that one credential rather
// than add another.
//
// It is signin:<person>|<machine account>#<box key thumbprint>, and all three parts are needed.
// The person is there because two people signed in on one box each reach their own secrets and
// must therefore hold a credential each, which one subject could not. The box key's thumbprint
// is there for the reason a machine's is, and because one person signed in on two boxes holds a
// sign-in on each, told apart by the key `agent-secrets login` made there and by nothing else —
// the machine account is a fleet's and is the same on every box of it. The machine account is
// there so that the credential can still be held to a listed fleet on every call
// (enroll.Service.AuthenticateLauncher): a person signs in from a box the deployment lists, and
// an account dropped from BROKER_VM_SERVICES ends its boxes' sign-in credentials as it ends
// their machines'.
func PersonSubject(person, account, thumbprint string) string {
	return signInSubjectPrefix + person + "|" + account + "#" + thumbprint
}

// SubjectKind says which of the identities this package keys a launcher credential by a subject
// names. The zero value is a subject this package did not write — a Kubernetes ServiceAccount's,
// system:serviceaccount:<namespace>:<name>, which the broker holds to a setting of its own — so
// a reader that tests for one kind never reads another's subject as that kind by forgetting the
// order to test them in.
type SubjectKind string

const (
	// SubjectOther is every subject neither MachineSubject nor PersonSubject wrote.
	SubjectOther SubjectKind = ""
	// SubjectMachine is a machine's own credential, which acts for no person.
	SubjectMachine SubjectKind = "machine"
	// SubjectSignIn is a person's sign-in on a machine, whose credential acts for that person.
	SubjectSignIn SubjectKind = "signin"
)

// Subject is a launcher credential's subject read back into the parts a reader acts on. The
// thumbprint the subject is keyed by, and the person a sign-in acts for, are checked here and
// kept by nothing: every reader of a subject acts on its kind and its account, and the broker
// keeps the person and the thumbprint in columns of their own.
type Subject struct {
	Kind SubjectKind
	// Account is the machine's Google account, which is what says the credential's fleet is
	// still listed (enroll.Service.AuthenticateLauncher). A sign-in carries the account of the
	// box it was made on, so one test holds both to the same listed fleet.
	Account string
}

// ReadSubject reads a launcher credential's subject back into its parts. It is the one reading
// of the grammar MachineSubject and PersonSubject write, so no caller spells that grammar a
// second time and none has to remember which kind to test for first: a subject it does not
// recognise, a malformed one included, comes back SubjectOther with nothing filled in.
func ReadSubject(subject string) Subject {
	if rest, signIn := strings.CutPrefix(subject, signInSubjectPrefix); signIn {
		person, box, split := strings.Cut(rest, "|")
		if !split {
			return Subject{}
		}
		account, _, keyed := strings.Cut(box, "#")
		if !keyed || person == "" || account == "" {
			return Subject{}
		}
		return Subject{Kind: SubjectSignIn, Account: account}
	}
	machine, _, keyed := strings.Cut(subject, "#")
	if !keyed {
		return Subject{}
	}
	if at := strings.LastIndex(machine, "@"); at > 0 && strings.Contains(machine[at+1:], "/") {
		return Subject{Kind: SubjectMachine, Account: machine[:at]}
	}
	if machine == "" {
		return Subject{}
	}
	return Subject{Kind: SubjectMachine, Account: machine}
}

// String names the scope as a refusal or a log line spells it, the zero value included, so a
// Claims that decided nothing does not read as a number.
func (s ProjectScope) String() string {
	switch s {
	case ProjectScopeNamed:
		return "the one project its entry names"
	case ProjectScopePerson:
		return "the projects its person may see that it names"
	case ProjectScopeNone:
		return "no project: reads only"
	case ProjectScopeResults:
		return "one named project's results only"
	default:
		return "no project scope"
	}
}

// Claims are what an exchange writes into the Dispatch access token it mints, and the principal
// the session name it carries belongs to.
type Claims struct {
	// Session is who the token's writes are recorded as.
	Session string
	// Person is who the session acts for: the human answerable for what the token writes. A
	// token names a person or a service and never both, so this is empty exactly where Service
	// is set.
	Person string
	// Service is the service a token that names no person is: the fleet a machine belongs to,
	// which is what a machine whose entry names no person gets instead of a person, or the
	// service a read entry names. It is empty for every other decision, each of which acts for a
	// person.
	Service string
	// Scope is what Projects means, and is always one of ProjectScopeNamed, ProjectScopePerson,
	// ProjectScopeNone or ProjectScopeResults: every constructor sets it, so a reader never has
	// to read an empty list as any of them. ProjectScopeNone is a read entry's, and is the one
	// scope under which the minted token holds the OAuth scope oauthwire.ScopeRead;
	// ProjectScopeResults is a result entry's, and is the one under which it holds
	// oauthwire.ScopeResults.
	Scope ProjectScope
	// Projects is the token's list of projects, sorted, each once: the entry's one project for a
	// ProjectScopeNamed token (none for an account entry naming none), for a ProjectScopePerson
	// token the projects its trade asked for, nil for none, the result entry's one project for a
	// ProjectScopeResults token, whose results alone it writes, and nil for a ProjectScopeNone
	// token, which names no project. The slice is this Claims' own.
	Projects []string
	// Principal is who the session name belongs to, and is not a claim of the minted token: it is
	// what the broker records as the name's holder, so a caller that may exchange cannot mint a
	// token naming a session another principal writes under. It is the listed account for a pod,
	// the listed workflow (its repository, path and ref) for a run, the listed workflow (its
	// repository and path) of a result entry for a run that writes results, the listed Google
	// account for a machine, and the person for a dev box session, each spelled with its kind
	// first (account:, workflow:, workflow-results:, google:, person:) so principals of two kinds
	// never compare equal.
	// Every pod of one account, every run of one workflow, every machine of one Google account
	// and every machine of one person is therefore one holder.
	// token naming a session another principal writes under, unless the broker pairs the caller's
	// account with that principal's (oauthstore.Store.SetSessionPairs), when the claim takes the
	// name. It is the listed account for a pod, the listed workflow (its repository, path and ref)
	// for a run, the listed Google account for a machine, and the person for a dev box session,
	// each spelled with its kind first (account:, workflow:, google:, person:) so principals of
	// two kinds never compare equal. Every pod of one account, every run of one workflow, every
	// machine of one Google account and every machine of one person is therefore one holder.
	Principal string
	// Instance is which caller of that principal is asking, and is not a claim of the minted
	// token either: it is the pod uid a registration has to name for this exchange to be made
	// (oauthstore.Store.PodSessionRegistered), and is empty for every caller no registration
	// separates from the others of its principal — a pod whose entry fixes the session, which
	// every read entry does, a workflow run, and a dev box session, each of which carries a name
	// its configuration or its person already decides.
	Instance string
	// Fleet is the service a machine's Google account is listed for (GoogleEntry.Service),
	// whether or not the token names it, and is empty for every other decision. Like Principal
	// and Instance it is no claim of the minted token: it is what the launcher credential the
	// exchange mints for that machine records as its service, which a credential keyed by a
	// subject must carry. Service holds the same string when the entry names no person, and
	// nothing when it names one, so a reader that wants the fleet of every machine reads this.
	Fleet string
	// Actor is the machine acting for the person in a delegation (RFC 8693 Section 2.1): the
	// Google account the request's actor token names, as BROKER_VM_SERVICES lists it. It is set
	// by Person alone and empty for every other decision, none of which has an actor. It is a
	// claim of the minted token in the sense that the provider writes it onto the stored session
	// and an introspection answers it (oauth.ClaimMachine), beside the RFC 8693 'act' claim the
	// library builds from the actor token's own subject; this is the account an operator reads.
	Actor string
	// CI is the run a result entry's token was minted for: the run's repository, workflow, ref
	// and commit, its event, its run and attempt, its runner and the check run of its job, as the
	// run's own GitHub token named them. It is set exactly when Scope is ProjectScopeResults, and
	// nil for every other decision. The provider writes it onto the stored session and an
	// introspection answers it (oauth.ClaimCI), so a resource server holds the token's writes to
	// that run's commit and job.
	CI *oauthwire.CIRun
}

// Reservation is a session name an entry fixes and the principal it belongs to. The broker holds
// each one for its entry's principal before it makes any exchange, so a name an entry fixes is
// refused to every other caller by the same rule that refuses any held name, whether or not the
// entry's own caller has exchanged yet.
type Reservation struct {
	Session   string
	Principal string
}

// AccountEntry lists one Kubernetes service account and what a pod running as it may write.
// Account and Person are required; Project and Session are not.
type AccountEntry struct {
	// Account is the subject a pod's projected token carries,
	// system:serviceaccount:<namespace>:<name>, named exactly: there is no wildcard.
	Account string
	// Project is the one project key the minted token may write, or empty for an entry that names
	// none: the shared public-session account, whose named tokens act for Person and name no
	// project. Only an account entry may leave it empty.
	Project string
	// Person is who the account's pods act for.
	Person string
	// Session, when set, is the session name every token minted for this account carries, and a
	// request naming any other is refused. It is how a long-running service keeps writing under
	// one name it does not get to choose per request. The name is one of the set's Reservations,
	// held for the account however many of its pods exchange and refused to every other caller.
	Session string
}

// ReadEntry lists one Kubernetes service account whose pods may read and may write nothing: the
// token they are minted holds the OAuth scope oauthwire.ScopeRead and the project scope
// ProjectScopeNone, names Service and Session, and names no person and no project. Every field is
// required. A resource server accepts such a token only where it asks to, and Dispatch serves it
// on the routes its read list names and nowhere else.
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
	// nobody whose session a request could name, and it is one of the set's Reservations, held
	// for the account as AccountEntry.Session is.
	Session string
}

// WorkflowEntry lists one GitHub workflow of one repository on one ref, and what a run of it may
// write. Every field but Session is required: nothing is wildcarded by leaving a field out, so an
// entry that names no ref matches no run rather than every one of them.
type WorkflowEntry struct {
	// Repository is <owner>/<name>, the repository the run belongs to.
	Repository string
	// Workflow is the workflow file's path inside the repository, such as
	// .github/workflows/release.yaml, compared exactly.
	Workflow string
	// Ref is the git ref the run is on, such as refs/heads/main, compared exactly.
	Ref string
	// Project is the one project key the minted token may write.
	Project string
	// Person is who the run acts for.
	Person string
	// Session, when set, is the session name every token minted for this workflow carries, as
	// AccountEntry.Session is for an account.
	Session string
}

// ResultWorkflowEntry lists one GitHub workflow of one repository whose runs, on each ref
// RefPattern matches, may write the results of their project's spec checks and nothing else.
// Every field is required. The token a run is minted holds the OAuth scope
// oauthwire.ScopeResults and the project scope ProjectScopeResults, acts for Person, writes under
// ResultSession(Repository, Workflow), and carries the run (Claims.CI). A resource server accepts
// such a token only where it asks to, and Dispatch serves it on the routes its results list
// names and nowhere else.
//
// It is a list of its own, BROKER_EXCHANGE_RESULT_WORKFLOWS, rather than a kind of WorkflowEntry,
// so that a broker of a build that predates it reads none of its entries and mints such a run
// nothing, where the same entry in BROKER_EXCHANGE_WORKFLOWS would read as a token that writes
// its project. A workflow is listed one way or the other, never both (NewSet).
type ResultWorkflowEntry struct {
	// Repository is <owner>/<name>, the repository the run belongs to.
	Repository string
	// Workflow is the workflow file's path inside the repository, such as
	// .github/workflows/ci-results.yaml, compared exactly.
	Workflow string
	// RefPattern is the refs whose runs the entry takes, as path.Match reads a pattern, with a
	// "*" matching one segment of the ref and never a "/": refs/heads/* is every branch with no
	// "/" in its name and refs/heads/*/* every branch with one. Its first two segments are
	// literal (CheckRefPattern), so one entry never reaches branches and tags at once. Two
	// entries of one workflow may name two patterns, and a run whose ref both match is refused
	// (ErrTwoEntries).
	RefPattern string
	// Project is the one project key whose results the minted token may write.
	Project string
	// Person is who the run acts for.
	Person string
}

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
// person and writes that one project, exactly as an AccountEntry does for a pod, and an entry
// that names neither mints a token that names its service and no person at all.
type GoogleEntry struct {
	// Account is the service account's email address, the `email` claim of its identity tokens,
	// named exactly: there is no wildcard.
	Account string
	// Service is the service a machine of this account is, which the minted token names when the
	// entry names no person.
	Service string
	// CloudProject, when set, is the Google Cloud project the machine must run in, compared
	// exactly with the token's google.compute_engine.project_id.
	CloudProject string
	// Instance, when set, is the pattern the machine's instance name must match, in path.Match's
	// syntax, such as example-vm-*.
	Instance string
	// Project, when set, is the one Dispatch project key the minted token may write. It is set
	// with Person and with nothing else.
	Project string
	// Person, when set, is who the machine's token acts for. An entry that names none mints a
	// token that names Service and no person.
	Person string
	// Session, when set, is the session name every token minted for this account carries, as
	// AccountEntry.Session is for an account.
	Session string
}

// workflowKey is the triple a workflow run is looked up by: every field of an entry is required,
// so the key is the whole identity and no partial match exists.
type workflowKey struct{ repository, workflow, ref string }

// resultKey is the pair a result entry is listed under: the repository and the workflow's path.
// The ref is matched against each of the pair's patterns, so it is no part of the key.
type resultKey struct{ repository, workflow string }

// Set is the listed identities as one build of the configuration read them, with the Google
// Workspace domain a person signing in on a machine must belong to. It is immutable once built,
// so every Pod, Workflow, Google, Person and Machine on it is a pure function of its arguments.
type Set struct {
	accounts     map[string]AccountEntry
	reads        map[string]ReadEntry
	workflows    map[workflowKey]WorkflowEntry
	results      map[resultKey][]ResultWorkflowEntry
	google       map[string]GoogleEntry
	signInDomain string
	reservations []Reservation
	// mapped is the person and project each listed principal's entry gives it, for
	// MappedProjects; a Google entry naming no person and a read entry map their principal to
	// neither.
	mapped map[string]mapping
}

// mapping is what an operator's entry gives the tokens of its principal: a person and one
// project, or, for a Google entry naming no person and for a read entry, neither.
type mapping struct{ person, project string }

// NewSet validates every entry and builds the set the exchange decides against. It refuses,
// naming the entry it refuses: an empty or blank required field, an account or repository that is
// not of its grammar, a fixed session name that is not usable, an identity listed twice, since
// two answers for one identity is a configuration whose meaning depends on order, two workflows
// whose principals spell alike, and a session name two entries fix, since one name belongs to one
// principal. A Google entry is refused on top of that for half a pin and for half a grant: one
// that pins a Compute Engine project without an instance pattern or the other way round says
// nothing clear about which machines it means, and one that names a person without a project, or
// a project without a person, says nothing clear about what they may write.
//
// reads are the read entries, which list Kubernetes service accounts as accounts does. An
// account both lists name is refused, naming its read entry, for the reason an identity listed
// twice is: it would have two answers, a token that writes as its person and one that only reads.
// A read entry's session is required as well as usable, and is reserved for the account as an
// account entry's fixed one is.
//
// results are the result entries, which list workflows by their repository and path and a ref
// pattern. It refuses, naming the entry: an empty or blank field, a repository not of its
// grammar, a pattern CheckRefPattern refuses, one workflow under one pattern twice, a workflow
// that workflows lists too, naming both entries, since its run would have two answers, and a
// workflow whose session (ResultSession) is not a usable name or is one another entry fixes. The
// session is reserved once for the workflow's principal, however many patterns it is listed
// under. Two patterns of one workflow that overlap are not refused here, since which refs they
// share is no finite list; a run whose ref both match is refused instead (Workflow).
//
// signInDomain is the Google Workspace domain BROKER_SIGNIN_DOMAIN names, which Person holds a
// signing-in person's own token to. It is a field of the set rather than an argument of that
// decision because it is configuration exactly as the entries are, and it is refused nothing
// here: an empty one admits nobody (oidc.WorkspaceLogin), which is what a broker told about no
// domain should do with a person's token.
func NewSet(accounts []AccountEntry, reads []ReadEntry, workflows []WorkflowEntry, results []ResultWorkflowEntry, machines []GoogleEntry, signInDomain string) (*Set, error) {
	set := &Set{
		accounts:     make(map[string]AccountEntry, len(accounts)),
		reads:        make(map[string]ReadEntry, len(reads)),
		workflows:    make(map[workflowKey]WorkflowEntry, len(workflows)),
		results:      make(map[resultKey][]ResultWorkflowEntry, len(results)),
		google:       make(map[string]GoogleEntry, len(machines)),
		signInDomain: signInDomain,
		mapped:       make(map[string]mapping, len(accounts)+len(reads)+len(workflows)+len(machines)),
	}
	fixedBy, spelledBy := map[string]string{}, map[string]string{}
	reserve := func(where, session, principal string) error {
		if session == "" {
			return nil
		}
		if other, fixed := fixedBy[session]; fixed {
			return fmt.Errorf("exchange: %s: %s fixes the same session name, and one name belongs to one identity", where, other)
		}
		fixedBy[session] = where
		set.reservations = append(set.reservations, Reservation{Session: session, Principal: principal})
		return nil
	}
	for i, entry := range accounts {
		where := fmt.Sprintf("account entry %d, %q", i+1, entry.Account)
		if !accountPattern.MatchString(entry.Account) {
			return nil, fmt.Errorf("exchange: %s: an account is system:serviceaccount:<namespace>:<name>", where)
		}
		if err := checkGrant(where, entry.Project, entry.Person, entry.Session, true); err != nil {
			return nil, err
		}
		if _, listed := set.accounts[entry.Account]; listed {
			return nil, fmt.Errorf("exchange: %s: the account is listed twice, and one account writes as one identity", where)
		}
		if err := reserve(where, entry.Session, AccountPrincipal(entry.Account)); err != nil {
			return nil, err
		}
		set.accounts[entry.Account] = entry
		set.mapped[AccountPrincipal(entry.Account)] = mapping{person: entry.Person, project: entry.Project}
	}
	for i, entry := range reads {
		where := fmt.Sprintf("read entry %d, %q", i+1, entry.Account)
		if !accountPattern.MatchString(entry.Account) {
			return nil, fmt.Errorf("exchange: %s: an account is system:serviceaccount:<namespace>:<name>", where)
		}
		if strings.TrimSpace(entry.Service) == "" {
			return nil, fmt.Errorf("exchange: %s: the service is required, since a read entry's token names its service and no person", where)
		}
		if !ValidSession(entry.Session) {
			return nil, fmt.Errorf("exchange: %s: the session is required, since a read entry fixes the session its token names, and %s", where, sessionRule)
		}
		if _, listed := set.reads[entry.Account]; listed {
			return nil, fmt.Errorf("exchange: %s: the account is listed twice, and one account reads as one identity", where)
		}
		if _, listed := set.accounts[entry.Account]; listed {
			return nil, fmt.Errorf("exchange: %s: an account entry lists the same account, and one account mints one kind of token", where)
		}
		if err := reserve(where, entry.Session, AccountPrincipal(entry.Account)); err != nil {
			return nil, err
		}
		set.reads[entry.Account] = entry
		set.mapped[AccountPrincipal(entry.Account)] = mapping{}
	}
	exactly := map[resultKey]string{}
	for i, entry := range workflows {
		where := fmt.Sprintf("workflow entry %d, %q", i+1, entry.Repository+"/"+entry.Workflow+"@"+entry.Ref)
		if !repositoryPattern.MatchString(entry.Repository) {
			return nil, fmt.Errorf("exchange: %s: a repository is <owner>/<name>", where)
		}
		if strings.TrimSpace(entry.Workflow) == "" {
			return nil, fmt.Errorf("exchange: %s: the workflow's path is required, since an entry with no path matches no run", where)
		}
		if strings.TrimSpace(entry.Ref) == "" {
			return nil, fmt.Errorf("exchange: %s: the ref is required, since an entry with no ref matches no run", where)
		}
		if err := checkGrant(where, entry.Project, entry.Person, entry.Session, false); err != nil {
			return nil, err
		}
		key := workflowKey{entry.Repository, entry.Workflow, entry.Ref}
		if _, listed := set.workflows[key]; listed {
			return nil, fmt.Errorf("exchange: %s: the workflow is listed twice, and one workflow writes as one identity", where)
		}
		principal := workflowPrincipal(key)
		if other, spelled := spelledBy[principal]; spelled {
			return nil, fmt.Errorf("exchange: %s: %s spells the same identity, so the two could not be told apart as holders of a session name", where, other)
		}
		spelledBy[principal] = where
		if err := reserve(where, entry.Session, principal); err != nil {
			return nil, err
		}
		set.workflows[key] = entry
		set.mapped[principal] = mapping{person: entry.Person, project: entry.Project}
		pair := resultKey{entry.Repository, entry.Workflow}
		if _, listed := exactly[pair]; !listed {
			exactly[pair] = where
		}
	}
	for i, entry := range results {
		where := fmt.Sprintf("result entry %d, %q", i+1, entry.Repository+"/"+entry.Workflow+"@"+entry.RefPattern)
		if !repositoryPattern.MatchString(entry.Repository) {
			return nil, fmt.Errorf("exchange: %s: a repository is <owner>/<name>", where)
		}
		if strings.TrimSpace(entry.Workflow) == "" {
			return nil, fmt.Errorf("exchange: %s: the workflow's path is required, since an entry with no path matches no run", where)
		}
		if err := CheckRefPattern(entry.RefPattern); err != nil {
			return nil, fmt.Errorf("exchange: %s: %w", where, err)
		}
		if err := checkGrant(where, entry.Project, entry.Person, "", false); err != nil {
			return nil, err
		}
		key := resultKey{entry.Repository, entry.Workflow}
		if other, listed := exactly[key]; listed {
			return nil, fmt.Errorf("exchange: %s: %s lists the same workflow, and one workflow mints one kind of token, one that writes its project or one that writes only its results", where, other)
		}
		listed := set.results[key]
		for _, earlier := range listed {
			if earlier.RefPattern == entry.RefPattern {
				return nil, fmt.Errorf("exchange: %s: the workflow is listed twice under one ref pattern, and one workflow run writes as one identity", where)
			}
		}
		if len(listed) == 0 {
			session := ResultSession(entry.Repository, entry.Workflow)
			if !ValidSession(session) {
				return nil, fmt.Errorf("exchange: %s: the entry writes under the session %s of its workflow, and %s", where, session, sessionRule)
			}
			if err := reserve(where, session, resultPrincipal(key)); err != nil {
				return nil, err
			}
		}
		set.results[key] = append(listed, entry)
	}
	for i, entry := range machines {
		where := fmt.Sprintf("google entry %d, %q", i+1, entry.Account)
		if !googleAccountPattern.MatchString(entry.Account) {
			return nil, fmt.Errorf("exchange: %s: a Google account is the lowercase email address its identity tokens carry", where)
		}
		if strings.TrimSpace(entry.Service) == "" {
			return nil, fmt.Errorf("exchange: %s: the service is required, since a machine whose entry names no person is its service and nothing else", where)
		}
		if (entry.CloudProject == "") != (entry.Instance == "") {
			return nil, fmt.Errorf("exchange: %s: an entry pins the Compute Engine project and the instance names together or neither (project %q, instance names %q)", where, entry.CloudProject, entry.Instance)
		}
		if _, err := path.Match(entry.Instance, ""); err != nil {
			return nil, fmt.Errorf("exchange: %s: the instance-name pattern %q is not a pattern: %w", where, entry.Instance, err)
		}
		if (entry.Project == "") != (entry.Person == "") {
			return nil, fmt.Errorf("exchange: %s: an entry names the person and the project together or neither (project %q, person %q)", where, entry.Project, entry.Person)
		}
		if entry.Person != "" {
			if err := checkGrant(where, entry.Project, entry.Person, entry.Session, false); err != nil {
				return nil, err
			}
		} else if entry.Session != "" && !ValidSession(entry.Session) {
			return nil, fmt.Errorf("exchange: %s: %s", where, sessionRule)
		}
		if _, listed := set.google[entry.Account]; listed {
			return nil, fmt.Errorf("exchange: %s: the account is listed twice, and one account writes as one identity", where)
		}
		if err := reserve(where, entry.Session, GooglePrincipal(entry.Account)); err != nil {
			return nil, err
		}
		set.google[entry.Account] = entry
		set.mapped[GooglePrincipal(entry.Account)] = mapping{person: entry.Person, project: entry.Project}
	}
	return set, nil
}

// Reservations are the session names the entries fix, each with the principal it belongs to, in
// the order the entries were listed: accounts first, then read entries, then workflows, then
// result entries, one for each workflow they list, then Google accounts. The slice is the
// caller's own, so the set stays immutable.
func (s *Set) Reservations() []Reservation {
	return slices.Clone(s.reservations)
}

// MappedProjects is the person and the projects an operator's entry gives the tokens of
// principal (AccountPrincipal, GooglePrincipal, or a workflow's), and false for a principal no
// entry lists, a person's own among them. A Google entry naming no person, and a read entry,
// answer neither, and true. It is how a reader holding no token — a release for a pod or a
// machine — reads the list that pod's or that machine's token would carry. The slice is the
// caller's own.
func (s *Set) MappedProjects(principal string) (person string, projects []string, ok bool) {
	entry, listed := s.mapped[principal]
	if !listed {
		return "", nil, false
	}
	if entry.project == "" {
		return entry.person, nil, true
	}
	return entry.person, []string{entry.project}, true
}

// ProjectsForAccount is the projects this configuration gives a token minted for one account,
// and false for an account no entry lists. An account that opens with "system:serviceaccount:"
// is read as a Kubernetes subject, any other as a Google account address, so a caller passes the
// string its own configuration holds and spells no principal. It reads the same entries Pod and
// Google mint from, so a grant made from this answer and the token of that account cannot
// disagree. A workflow is no account and is answered false.
func (s *Set) ProjectsForAccount(account string) ([]string, bool) {
	principal := GooglePrincipal(account)
	if strings.HasPrefix(account, "system:serviceaccount:") {
		principal = AccountPrincipal(account)
	}
	_, projects, ok := s.MappedProjects(principal)
	return projects, ok
}

// entryProjects is the list a token an operator's entry maps carries: that entry's one project,
// or none for an account entry that names none. A request may name the entry's project or name
// nothing; one naming anything else is refused, since the entry is what chose the project and a
// trade may neither widen nor change it.
func entryProjects(identity, project string, asked []string) ([]string, error) {
	if project == "" {
		if len(asked) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %s's entry names no project, and the request named %s",
			ErrProjectsFixed, identity, oauthwire.JoinProjects(asked))
	}
	if len(asked) == 0 || (len(asked) == 1 && asked[0] == project) {
		return []string{project}, nil
	}
	return nil, fmt.Errorf("%w: %s writes the project %s that its entry names, and the request named %s",
		ErrProjectsFixed, identity, project, oauthwire.JoinProjects(asked))
}

// checkGrant refuses what every entry shares: a blank person, which would mint a token answerable
// to nobody, and a fixed session name no request could name back. The project is required too,
// except where noProject allows an empty one: an account entry may name a person and no project,
// which is the shared public-session account's entry, minting named tokens that name no project
// (a pod account many people launch must never act as one person's own token). A project that is
// blank without being empty is refused everywhere, and so is one that is no project key: every
// token the entry mints names it, the exchange stores and reads that list through
// oauthwire.ParseProjects, which takes only project keys, so an entry naming anything else would
// boot and then fail every trade.
func checkGrant(where, project, person, session string, noProject bool) error {
	if !(noProject && project == "") && strings.TrimSpace(project) == "" {
		return fmt.Errorf("exchange: %s: the project is required, since the minted token may write one project", where)
	}
	if project != "" {
		if err := oauthwire.ValidProjects([]string{project}); err != nil {
			return fmt.Errorf("exchange: %s: the entry's project %v", where, err)
		}
	}
	if strings.TrimSpace(person) == "" {
		return fmt.Errorf("exchange: %s: the person is required, since every token acts for someone", where)
	}
	if session != "" && !ValidSession(session) {
		return fmt.Errorf("exchange: %s: %s", where, sessionRule)
	}
	return nil
}

// Pod answers the claims for a pod's Kubernetes service-account token. The token's own shape is
// read first — the account it was minted for, and the pod it is bound to — and the entry for that
// account second, so an operator reading the refusal learns which of the two to fix.
//
// A token carrying no pod uid is refused however the account is listed: Kubernetes mints such a
// token to anyone holding the account's credentials, and it proves only that the account exists,
// where a bound one proves Kubernetes minted it for one pod of this cluster. It does not prove
// that pod still runs: the broker's exchange verifies the token offline against the issuer's
// published keys and asks the cluster nothing, so a token whose pod has been deleted is exchanged
// until its own expiry, which the Kubernetes subject token's lifetime tolerance
// (oauth.kubernetesTolerance) allows up to 24 hours ahead. What bounds the reach that follows is
// the access token each exchange mints, which lives BROKER_ACCESS_TOKEN_SECONDS (15 minutes by
// default). That trades immediate revocation of a pod's token for needing no access to the
// cluster's own interface.
//
// The principal is the account, not the pod: every pod of one account is one holder, so the
// replacement pod of a rolling update, or a Legion session resumed in a relaunched pod, keeps
// writing under the name the pod before it held. What keeps the pods of one account apart from
// one another is therefore not the claim on the name but Instance, the pod uid, which this
// answers for an entry that fixes no session: the exchange mints nothing under that name unless a
// registration its deployment made says that pod uid may use it, and a relaunch is a second pod
// uid registered for the same name rather than a refusal. An entry that fixes a session answers
// no Instance and needs no registration, because the name its tokens carry is the
// configuration's, the same for every pod of the account.
//
// A read entry always fixes its session, so its pods are answered the same way, with the claims
// of a token that writes nothing: the entry's service and session, ProjectScopeNone, and no
// person, no project and no Instance. Its token names no person, so a request of its pod naming
// a project is refused (ErrProjectsNoPerson), as a machine's whose entry names no person is.
func (s *Set) Pod(token map[string]any, session string, projects []string) (Claims, error) {
	account, err := claimString(token, "sub")
	if err != nil {
		return Claims{}, err
	}
	uid := podUID(token)
	listing, err := s.podEntry(account, uid)
	if err != nil {
		return Claims{}, err
	}
	name, err := resolveSession(listing.fixed(), session, account)
	if err != nil {
		return Claims{}, err
	}
	if listing.readOnly {
		if len(projects) > 0 {
			return Claims{}, fmt.Errorf("%w: %s reads as the service %s and names no person, and the request named %s",
				ErrProjectsNoPerson, account, listing.read.Service, oauthwire.JoinProjects(projects))
		}
		return Claims{Session: name, Service: listing.read.Service, Scope: ProjectScopeNone, Principal: AccountPrincipal(account)}, nil
	}
	entry := listing.account
	named, err := entryProjects(account, entry.Project, projects)
	if err != nil {
		return Claims{}, err
	}
	claims := Claims{Session: name, Person: entry.Person, Scope: ProjectScopeNamed, Projects: named, Principal: AccountPrincipal(account)}
	if entry.Session == "" {
		claims.Instance = uid
	}
	return claims, nil
}

// podListing is the entry that lists a pod's account: its AccountEntry, or, when readOnly is set,
// its ReadEntry. NewSet refuses an account both lists name, so exactly one of them answers.
type podListing struct {
	account  AccountEntry
	read     ReadEntry
	readOnly bool
}

// fixed is the session name the listing fixes: a read entry's always, an account entry's when it
// fixes one, and empty for an account entry that leaves the name to the request.
func (l podListing) fixed() string {
	if l.readOnly {
		return l.read.Session
	}
	return l.account.Session
}

// podEntry is the reading of a pod's identity that Pod and Registration both begin with: the
// pod uid the token must carry, refused first because a token bound to no pod proves only that
// the account exists, and then the entry that lists the account, an account entry or a read
// entry. Both refuse in this order, in these words, because they are this one helper: a
// registrar and the exchange that reads its row refuse the same pods for the same reasons, which
// Registration's doc promises.
func (s *Set) podEntry(account, uid string) (podListing, error) {
	if uid == "" {
		return podListing{}, fmt.Errorf("%w: %s presented a token bound to no pod, which proves only that the account exists", ErrNoPodBinding, account)
	}
	if entry, listed := s.accounts[account]; listed {
		return podListing{account: entry}, nil
	}
	if entry, listed := s.reads[account]; listed {
		return podListing{read: entry, readOnly: true}, nil
	}
	return podListing{}, fmt.Errorf("%w: no entry lists the service account %s", ErrNoEntry, account)
}

// Registration refuses what a pod-session registration must refuse before it is written: a token
// bound to no pod, an account no entry lists, and an entry that fixes a session, which needs no
// registration because its name is the configuration's. A read entry is always that last: it
// fixes the session its token names. The first two refusals are podEntry, the one reading of a
// pod's identity Pod makes too, so a registrar and the exchange that reads its row refuse the
// same pods for the same reasons; what it does not decide is whether the name belongs to the
// account, which only the claim on the name answers (oauthstore.Store.BindSession).
func (s *Set) Registration(account, podUID, session string) error {
	listing, err := s.podEntry(account, podUID)
	if err != nil {
		return err
	}
	if fixed := listing.fixed(); fixed != "" {
		return fmt.Errorf("%w: %s's tokens all name the session %s, which every pod of it may use, so its pods are registered for no name", ErrSessionFixed, account, fixed)
	}
	if !ValidSession(session) {
		return fmt.Errorf("%w: the registration of %s names a session of %d bytes; %s", ErrSessionInvalid, account, len(session), sessionRule)
	}
	return nil
}

// Workflow answers the claims for a continuous-integration workflow run's GitHub token, from the
// claims GitHub puts in it: `repository`, `workflow_ref`, `job_workflow_ref` and `ref`, and, for
// a run of a result entry's workflow, the run's own claims (readCIRun).
//
// The identity is the repository, the workflow file's path and the ref. The path is the
// workflow_ref without the repository and without "@" and the ref (oauthwire.WorkflowPath), since
// a workflow file's name and a branch's name can each hold an "@"; a workflow_ref that is not of
// that form is refused, naming it and the ref. A run whose job comes from a reusable workflow
// other than the one the run started in is refused: `job_workflow_ref` names the workflow file
// the job is written in and `workflow_ref` the one the run entered through, and where they differ
// the code that is running is not the code the entry names. An entry that lists the workflow on
// that exact ref answers a token that writes its project; a workflow no such entry lists is
// looked for among the result entries (resultRun).
//
// GitHub's token of a job that is written directly in its workflow holds `job_workflow_ref` equal
// to `workflow_ref` (measured on real runs on 2026-11-16, and Sigstore's Fulcio requires the claim
// of every GitHub Actions token, https://github.com/sigstore/fulcio/blob/main/docs/oidc.md#github),
// so a token with no such claim is refused (ErrClaimMissing).
//
// The principal is that listed workflow, not the run: every run of it is one holder, so the next
// run of a release workflow keeps writing under the name the run before it held, however soon
// after it starts.
func (s *Set) Workflow(token map[string]any, session string, projects []string) (Claims, error) {
	repository, err := claimString(token, "repository")
	if err != nil {
		return Claims{}, err
	}
	workflowRef, err := claimString(token, "workflow_ref")
	if err != nil {
		return Claims{}, err
	}
	jobRef, err := claimString(token, "job_workflow_ref")
	if err != nil {
		return Claims{}, err
	}
	ref, err := claimString(token, "ref")
	if err != nil {
		return Claims{}, err
	}
	if jobRef != workflowRef {
		return Claims{}, fmt.Errorf("%w: %s ran %s, whose job is written in %s; an entry lists the workflow a job runs in", ErrNoEntry, repository, workflowRef, jobRef)
	}
	workflow, ok := oauthwire.WorkflowPath(workflowRef, repository, ref)
	if !ok {
		return Claims{}, fmt.Errorf("%w: the subject token's workflow_ref %s is not %s/<path>@%s: its repository, the path of its workflow file and its ref", ErrClaimMissing, workflowRef, repository, ref)
	}
	key := workflowKey{repository, workflow, ref}
	entry, listed := s.workflows[key]
	if !listed {
		return s.resultRun(token, resultKey{repository, workflow}, workflowRef, ref, session, projects)
	}
	name, err := resolveSession(entry.Session, session, repository+"/"+workflow)
	if err != nil {
		return Claims{}, err
	}
	named, err := entryProjects(repository+"/"+workflow, entry.Project, projects)
	if err != nil {
		return Claims{}, err
	}
	return Claims{Session: name, Person: entry.Person, Scope: ProjectScopeNamed, Projects: named, Principal: workflowPrincipal(key)}, nil
}

// resultRun answers the claims for a run of a workflow the result entries list under key, on ref:
// the entry whose pattern matches ref gives the project and the person, the session is the
// workflow's own (ResultSession), which a request may name or leave out, the principal is the
// workflow whatever its ref, and the token carries the run (readCIRun). A ref no pattern
// matches is no entry's run, and one two patterns match is refused naming both, since the run
// would have two answers. The request may name the entry's project or nothing, as for every
// entry that fixes its project (entryProjects).
func (s *Set) resultRun(token map[string]any, key resultKey, workflowRef, ref, session string, projects []string) (Claims, error) {
	var matched []ResultWorkflowEntry
	for _, entry := range s.results[key] {
		// NewSet refused every pattern path.Match cannot read, so the error is never set.
		if ok, _ := path.Match(entry.RefPattern, ref); ok {
			matched = append(matched, entry)
		}
	}
	switch len(matched) {
	case 0:
		return Claims{}, fmt.Errorf("%w: no entry lists the workflow %s of %s on %s", ErrNoEntry, key.workflow, key.repository, ref)
	case 1:
	default:
		patterns := make([]string, len(matched))
		for i, entry := range matched {
			patterns[i] = entry.RefPattern
		}
		return Claims{}, fmt.Errorf("%w: the result entries of the workflow %s of %s under the ref patterns %s all match %s; list the workflow under patterns no ref matches two of", ErrTwoEntries, key.workflow, key.repository, strings.Join(patterns, ", "), ref)
	}
	run, err := readCIRun(token, key.repository, workflowRef, ref)
	if err != nil {
		return Claims{}, err
	}
	name, err := resolveSession(ResultSession(key.repository, key.workflow), session, key.repository+"/"+key.workflow)
	if err != nil {
		return Claims{}, err
	}
	entry := matched[0]
	named, err := entryProjects(key.repository+"/"+key.workflow, entry.Project, projects)
	if err != nil {
		return Claims{}, err
	}
	return Claims{Session: name, Person: entry.Person, Scope: ProjectScopeResults, Projects: named, Principal: resultPrincipal(key), CI: &run}, nil
}

// readCIRun reads the run a result entry's token carries out of the run's GitHub token: the
// repository, workflow and ref Workflow has already read, and the `sha`, `event_name`, `run_id`,
// `run_attempt`, `runner_environment` and `check_run_id` claims, every one of them required. The
// commit is in the form oauthwire.IsCommit holds, and the run, the attempt and the check run in
// the form oauthwire.IsPositiveDecimal holds, the forms a resource server admits. The check run
// is the job's identity, which a resource server reads the job's name from, so a token without
// it is refused rather than minted a token naming no job.
func readCIRun(token map[string]any, repository, workflowRef, ref string) (oauthwire.CIRun, error) {
	run := oauthwire.CIRun{Repository: repository, WorkflowRef: workflowRef, Ref: ref}
	for _, claim := range []struct {
		name  string
		into  *string
		check func(string) bool
		form  string
	}{
		{name: "sha", into: &run.SHA, check: oauthwire.IsCommit, form: "40 lowercase hex digits"},
		{name: "event_name", into: &run.EventName},
		{name: "run_id", into: &run.RunID, check: oauthwire.IsPositiveDecimal, form: "a positive decimal number a signed 64-bit integer holds"},
		{name: "run_attempt", into: &run.RunAttempt, check: oauthwire.IsPositiveDecimal, form: "a positive decimal number a signed 64-bit integer holds"},
		{name: "runner_environment", into: &run.RunnerEnvironment},
		{name: "check_run_id", into: &run.CheckRunID, check: oauthwire.IsPositiveDecimal, form: "a positive decimal number a signed 64-bit integer holds"},
	} {
		value, err := claimString(token, claim.name)
		if err != nil {
			return oauthwire.CIRun{}, err
		}
		if claim.check != nil && !claim.check(value) {
			return oauthwire.CIRun{}, fmt.Errorf("%w: the subject token's %s claim is not %s", ErrClaimMissing, claim.name, claim.form)
		}
		*claim.into = value
	}
	return run, nil
}

// CheckRefPattern refuses, naming it, a ref pattern a result entry cannot list: one path.Match
// cannot read, or one that is not refs/, a kind of ref spelled out, and a pattern of at least one
// more segment, with no segment empty. The first two segments are literal so that one pattern
// never reaches two kinds of ref, branches and tags, nor a string that is no ref.
func CheckRefPattern(pattern string) error {
	segments := strings.Split(pattern, "/")
	switch {
	case len(segments) < 3 || slices.Contains(segments, ""):
		return fmt.Errorf("the ref pattern %q is not refs/<kind>/<pattern>, with no segment empty", pattern)
	case segments[0] != "refs" || strings.ContainsAny(segments[1], `*?[\`):
		return fmt.Errorf("the ref pattern %q does not begin with refs/ and a kind of ref spelled out, such as refs/heads/, so it could match a ref of every kind", pattern)
	}
	if _, err := path.Match(pattern, ""); err != nil {
		return fmt.Errorf("the ref pattern %q is not a pattern: %w", pattern, err)
	}
	return nil
}

// ResultSession is the session a result entry's token writes under: ci-results:<repository>/<the
// workflow's path>, one name for every run of the workflow, whatever its ref and its pattern.
func ResultSession(repository, workflow string) string {
	return "ci-results:" + repository + "/" + workflow
}

// Google answers the claims for a machine's Google identity token, from the claims Google puts in
// it: `email`, `email_verified` and, for a token minted on a Compute Engine instance with
// format=full, the `google` → `compute_engine` members.
//
// The identity is the account the token was minted for, which an entry lists by its email
// address. Google's signature, the issuer, the audience and the expiry are checked before this is
// reached (oauth.ExchangeConfig); what is left to decide is that Google holds the address
// verified, that the machine is one the entry allows, and what its token may write.
//
// Where a machine runs is the entry's to pin. An entry that pins the Compute Engine project and
// an instance-name pattern holds the token to an instance of that project whose name the pattern
// matches, and refuses a token carrying no Compute Engine claims, which proves nothing about
// where it was minted. An entry that pins neither takes such a token: that is what a token minted
// through a workload identity pool carries, and what a token from the metadata server of a pod on
// Google Kubernetes Engine carries, neither of which runs on an instance of its own.
//
// The principal is the listed account, not the machine: every machine of one account is one
// holder, so a machine rebuilt under another instance id keeps writing under the name the machine
// before it held, and a machine of any other account is refused that name. Nothing separates two
// machines of one account the way a registration separates two pods, so each names a session of
// its own and the claim on the name keeps them apart.
//
// An entry that names a person and a project mints a token that acts for that person and names
// that project, as a pod's entry does. An entry that names neither mints a token that names its
// service and no person at all: it names no project either, and says so with the person scope,
// the wire's word for a token no entry maps to a project. Nothing it can write follows from that
// alone — a resource server holds a token naming no person to what a service may do — which is
// why the service is a claim of its own rather than an absence to be read, and why a request of
// such a machine naming a project is refused rather than read.
func (s *Set) Google(token map[string]any, session string, projects []string) (Claims, error) {
	identity := ReadGoogleIdentity(token)
	if identity.Account == "" {
		return Claims{}, fmt.Errorf("%w: the subject token carries no email claim, so it names no account (ask for it with format=full)", ErrClaimMissing)
	}
	entry, listed := s.google[identity.Account]
	if !listed {
		return Claims{}, fmt.Errorf("%w: no entry lists the Google account %s", ErrNoEntry, identity.Account)
	}
	if !identity.EmailVerified {
		return Claims{}, fmt.Errorf("%w: %s presented a token whose email_verified is not true", ErrEmailNotVerified, identity.Account)
	}
	if err := s.allowedMachine(entry, identity); err != nil {
		return Claims{}, err
	}
	name, err := resolveSession(entry.Session, session, identity.Account)
	if err != nil {
		return Claims{}, err
	}
	claims := Claims{Session: name, Scope: ProjectScopePerson, Principal: GooglePrincipal(identity.Account), Fleet: entry.Service}
	if entry.Person == "" {
		if len(projects) > 0 {
			return Claims{}, fmt.Errorf("%w: %s is the service %s and names no person, and the request named %s",
				ErrProjectsNoPerson, identity.Account, entry.Service, oauthwire.JoinProjects(projects))
		}
		claims.Service = entry.Service
		return claims, nil
	}
	if claims.Projects, err = entryProjects(identity.Account, entry.Project, projects); err != nil {
		return Claims{}, err
	}
	claims.Person, claims.Scope = entry.Person, ProjectScopeNamed
	return claims, nil
}

// allowedMachine holds a token to the Compute Engine instance its entry pins, and takes every
// machine of an entry that pins nothing. The two refusals are told apart because an operator acts
// on each differently: a token with no Compute Engine claims was minted somewhere the pins cannot
// describe, and a token with them from another project or another instance name is a machine the
// entry does not list.
func (s *Set) allowedMachine(entry GoogleEntry, identity GoogleIdentity) error {
	if entry.CloudProject == "" && entry.Instance == "" {
		return nil
	}
	if !identity.Compute.Whole() {
		return fmt.Errorf("%w: %s presented a token carrying no Compute Engine identity, and its entry takes machines of the project %s named %s",
			ErrNoComputeEngine, identity.Account, entry.CloudProject, entry.Instance)
	}
	if identity.Compute.Project != entry.CloudProject {
		return fmt.Errorf("%w: %s presented a token minted in the project %s, and its entry takes machines of %s",
			ErrMachineNotAllowed, identity.Account, identity.Compute.Project, entry.CloudProject)
	}
	// The pattern was checked at NewSet, so a match error here is impossible; a pattern that did
	// not match is the refusal either way.
	if matched, _ := path.Match(entry.Instance, identity.Compute.Instance); !matched {
		return fmt.Errorf("%w: %s presented a token minted on the instance %s, and its entry takes the names %s",
			ErrMachineNotAllowed, identity.Account, identity.Compute.Instance, entry.Instance)
	}
	return nil
}

// Person answers the claims for a person's own Google identity token, presented with the
// machine's own as the actor token (RFC 8693 Section 2.1): the sign-in `agent-secrets login`
// made on that machine, where the person is the subject and the machine is only acting for
// them.
//
// The two tokens are read by the two rules that belong to them. The person's is held to the
// Workspace rule the broker's own approval sign-in is held to (oidc.WorkspaceLogin): a subject,
// an email of an address's shape that Google has verified, and the `hd` claim equal to the
// configured domain, which an empty configured domain can never match. The domain comes from
// `hd` alone, never from the address's suffix, because a personal Google account can hold an
// address under a Workspace's domain. Google's signature, the issuer, the expiry and the
// audience of the device-flow client are checked before this is reached
// (oauth.ExchangeConfig.Person). The machine's is held to the entry rules a machine trading for
// itself is held to: its account is one BROKER_VM_SERVICES lists, Google holds that address
// verified, and the machine runs where the entry pins it, so a person cannot sign in from a box
// of an account the deployment does not list.
//
// The principal is the person, as it is for a dev box session's machine credential: every
// machine and every session of one person is one holder, so a person keeps their session names
// from box to box, and no other identity may write under one of them. The token acts as that
// person, with the person scope, and carries the projects the trade asked for: a person's sign-in
// is their own identity on their own box, which reaches whichever projects that person reaches,
// and the list says which of them this session works on. That the person may see each one is the
// exchange's next question, which it asks Dispatch (internal/cipher/oauth); this package reads
// no Dispatch.
//
// What it does not decide is whether that person's sign-in on that box key still stands: the
// box key's thumbprint is the DPoP proof's, which this package never sees, and the sign-in is a
// row the provider reads in the exchange's own transaction
// (oauth.ExchangeConfig.PersonSignIns).
func (s *Set) Person(token, actor map[string]any, session string, projects []string) (Claims, error) {
	if actor == nil {
		return Claims{}, fmt.Errorf("%w: a person signs in on a machine, so the machine's own Google identity token is presented as the actor token (%s)", ErrNoActorToken, oauthwire.ActorTokenTypeMachine)
	}
	machine := ReadGoogleIdentity(actor)
	if machine.Account == "" {
		return Claims{}, fmt.Errorf("%w: the actor token carries no email claim, so it names no machine (ask for it with format=full)", ErrClaimMissing)
	}
	entry, listed := s.google[machine.Account]
	if !listed {
		return Claims{}, fmt.Errorf("%w: no entry lists the Google account %s, which the actor token names as the machine", ErrNoEntry, machine.Account)
	}
	if !machine.EmailVerified {
		return Claims{}, fmt.Errorf("%w: the machine %s presented an actor token whose email_verified is not true", ErrEmailNotVerified, machine.Account)
	}
	if err := s.allowedMachine(entry, machine); err != nil {
		return Claims{}, err
	}
	login, err := oidc.WorkspaceLogin(personClaims(token), s.signInDomain)
	switch {
	case errors.Is(err, oidc.ErrNoSubject):
		return Claims{}, fmt.Errorf("%w: the subject token carries no sub claim, so it names no person", ErrClaimMissing)
	case errors.Is(err, oidc.ErrNoEmail):
		return Claims{}, fmt.Errorf("%w: the subject token carries no email claim of an address's shape, so it names no person", ErrClaimMissing)
	case errors.Is(err, oidc.ErrNotInDomain):
		return Claims{}, fmt.Errorf("%w: %s signed in, and this broker takes the Google Workspace domain BROKER_SIGNIN_DOMAIN names; sign in again with `agent-secrets login` as an account of that domain", ErrNotInDomain, login)
	case errors.Is(err, oidc.ErrEmailNotVerified):
		return Claims{}, fmt.Errorf("%w: %s presented a token whose email_verified is not true", ErrEmailNotVerified, login)
	case err != nil:
		return Claims{}, fmt.Errorf("%w: the subject token is not a person's Google identity token (%v)", ErrClaimMissing, err)
	}
	name, err := resolveSession("", session, login)
	if err != nil {
		return Claims{}, err
	}
	return Claims{
		Session:   name,
		Person:    login,
		Scope:     ProjectScopePerson,
		Projects:  slices.Clone(projects),
		Principal: PersonPrincipal(login),
		Actor:     machine.Account,
		// The fleet the box belongs to, which the launcher credential the exchange mints for
		// this sign-in records as its service: a credential keyed by a subject carries one.
		// Nothing of the fleet reaches the token's claims — the token is the person's.
		Fleet: entry.Service,
	}, nil
}

// personClaims is the reading of a person's Google identity token that oidc.WorkspaceLogin
// judges: the four claims its rule names, out of the claim map of a token the exchange's own
// verifier has already checked. It is the one place those claim names are spelled for a person,
// as ReadGoogleIdentity is for a machine; `email_verified` is read through the same isVerified,
// since Google writes it as a boolean on some of its tokens and as the string "true" on others.
func personClaims(token map[string]any) oidc.Claims {
	subject, _ := token["sub"].(string)
	email, _ := token["email"].(string)
	domain, _ := token["hd"].(string)
	return oidc.Claims{
		Subject:       subject,
		Email:         email,
		EmailVerified: isVerified(token["email_verified"]),
		HostedDomain:  domain,
	}
}

// Machine answers the claims for a dev box session's machine credential, credential being the
// credential's own id. No entry lists one: the person is approver, the one the credential already
// records, and the session is the one the caller names. A machine login's token names the
// credential it was issued under, so a token naming none is refused as not being one.
//
// The principal is the person, not the credential: every machine and every session of one person
// is one holder, so a person's session resumed on another of their machines, or after their
// machine logged in again, keeps its name, and one person's dev box cannot write under another's
// session name or under one an entry fixes.
//
// The token acts as its person, and says so: its scope is ProjectScopePerson, and it carries the
// projects the trade asked for. A machine credential is a person's own identity on their own
// machine, as their browser cookie and their personal token are: the person reaches whichever
// projects they may reach, and the list says which of them this session works on, which the
// exchange holds to what Dispatch says that person may see before it mints. Naming the scope is
// what keeps a reader of an introspection answer from mistaking an empty list for a token no
// project check applies to.
func (s *Set) Machine(credential, approver, session string, projects []string) (Claims, error) {
	if strings.TrimSpace(credential) == "" {
		return Claims{}, fmt.Errorf("%w: the subject token names no machine credential id, so it is not a machine login's token", ErrClaimMissing)
	}
	if strings.TrimSpace(approver) == "" {
		return Claims{}, fmt.Errorf("%w: the machine credential records no approver to act for", ErrClaimMissing)
	}
	name, err := resolveSession("", session, approver)
	if err != nil {
		return Claims{}, err
	}
	return Claims{Session: name, Person: approver, Scope: ProjectScopePerson, Projects: slices.Clone(projects), Principal: PersonPrincipal(approver)}, nil
}

// The five spellings of a principal, each led by its kind so that no account, workflow, result
// workflow, Google account and person ever spell the same string. A workflow's is
// <repository>/<path>@<ref>, as an operator reads one elsewhere; a path or ref carrying "@" could
// make two entries spell one string, and NewSet refuses that pair rather than let one's runs hold
// the other's names. A result workflow's is <repository>/<path>, since every ref of it is one
// holder, and a repository holds exactly one "/", so no two pairs spell it alike; it is
// oauthwire.ResultPrincipal, since Dispatch stores it as the recorder of a result.
//
// Three of them are exported because a broker grant follows a session name under one principal
// too: enroll.Service.Create writes one of these into enrollments.carry_principal, and the next
// enrollment of that name under that principal takes what the last one held. The name a session
// writes its Dispatch tokens under and the name its grant follows are one name, so one holder has
// to spell the same either way, which it does only while there is one definition of each
// spelling. A pod's enrollment is the one exception, whose grant follows a pair rather than its
// account alone (PodLaunchPrincipal), while its name is held under the account. A pod of an
// account the broker pairs with that account (enroll.SessionBinder.PairedWith) takes what the
// other account's pods of the name hold only under the same login account, the pair LaunchedBy
// spells from the paired account's principal.

// AccountPrincipal is a Kubernetes ServiceAccount's spelling: the account a pod's own projected
// token proves.
func AccountPrincipal(account string) string { return "account:" + account }

func workflowPrincipal(key workflowKey) string {
	return "workflow:" + key.repository + "/" + key.workflow + "@" + key.ref
}

func resultPrincipal(key resultKey) string {
	return oauthwire.ResultPrincipal(key.repository, key.workflow)
}

// GooglePrincipal is a Google account's spelling: the fleet a machine's own identity belongs to.
func GooglePrincipal(account string) string { return "google:" + account }

// PersonPrincipal is a person's spelling, by the login that names them.
func PersonPrincipal(person string) string { return "person:" + person }

// PodLaunchPrincipal is the principal a pod's enrollment carries its grants under: the account
// the pod's own projected token proved, paired with launcher, the principal of the token login
// that enrolled the pod (AccountPrincipal of the account that login proved). Every pod of one
// account is one principal, and the pods of several daemons can run as one account (every public
// Legion project's pods run as legion-worker), so a session name's grant must follow the daemon
// that enrolled it too: a pod of another daemon, enrolled under the same name, takes nothing
// (plan E's t-e-54). It is never a name's holder: a pod's session name is
// held under its account alone, the principal its own token exchange claims the name under, so
// the pod's enrollment and its trade never refuse each other. No account or principal kind holds
// a space, so no two pairs spell one string. The broker's migration 0037_pod_carry_launcher
// writes the same spelling in SQL.
func PodLaunchPrincipal(account, launcher string) string {
	return LaunchedBy(AccountPrincipal(account), launcher)
}

// LaunchedBy is PodLaunchPrincipal spelled from an account's principal rather than the account:
// the pair a pod of the account accountPrincipal names carries its grants under when the login
// launcher names enrolled it. It is how enroll's adopt spells a paired account's pair, which the
// broker holds as a principal (enroll.SessionBinder.PairedWith), so the pair is spelled in one
// place.
func LaunchedBy(accountPrincipal, launcher string) string {
	return accountPrincipal + " launched-by " + launcher
}

// sessionRule is the one sentence that says what a session name may be, so an entry refused at
// build time and a request refused at exchange time are refused in the same words, and the cap it
// quotes is the cap ValidSession applies.
var sessionRule = fmt.Sprintf("a session name is printable, carries no whitespace, and is at most %d bytes", MaxSessionBytes)

// resolveSession answers the session claim for an entry whose fixed session is fixed and a request
// that named named, where identity names who is asking. An entry that fixes a session answers it
// and refuses a request that names another, rather than quietly putting the fixed one in its
// place; an entry that fixes none has nothing to fall back on, so the request must name a usable
// session.
//
// A refused name is described and never repeated: it comes from the request, so its length and
// its bytes are whatever the caller sent.
func resolveSession(fixed, named, identity string) (string, error) {
	if fixed != "" {
		if named != "" && named != fixed {
			return "", fmt.Errorf("%w: %s's tokens name the session %s", ErrSessionMismatch, identity, fixed)
		}
		return fixed, nil
	}
	if named == "" {
		return "", fmt.Errorf("%w: %s is listed with no session of its own, so the request names one", ErrSessionRequired, identity)
	}
	if !ValidSession(named) {
		return "", fmt.Errorf("%w: %s named a session of %d bytes; %s", ErrSessionInvalid, identity, len(named), sessionRule)
	}
	return named, nil
}

// ValidSession reports whether name is usable as a session claim: UTF-8, printable, with no
// whitespace and at most MaxSessionBytes. A pod-session registration of a name it refuses is
// refused before it is written (Registration), so no registration names one, and a release of
// registrations (oauthstore.Store.ReleasePodSessions) drops such a name as one that matches
// nothing rather than refusing it.
func ValidSession(name string) bool {
	if name == "" || len(name) > MaxSessionBytes || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// claimString reads one claim the decision needs as a non-empty string. A claim that is absent,
// null, or of another JSON type is the same fault to a caller: the subject token is not the kind
// of token this decision is for.
func claimString(token map[string]any, name string) (string, error) {
	value, present := token[name]
	if !present {
		return "", fmt.Errorf("%w: the subject token carries no %s claim", ErrClaimMissing, name)
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return "", fmt.Errorf("%w: the subject token's %s claim is not a non-empty string", ErrClaimMissing, name)
	}
	return text, nil
}

// podUID is the uid of the pod a projected service-account token is bound to (kubernetes.io → pod
// → uid), and "" for a token bound to no pod, such as one kubectl create token mints.
func podUID(token map[string]any) string {
	kubernetes, _ := token["kubernetes.io"].(map[string]any)
	pod, _ := kubernetes["pod"].(map[string]any)
	uid, _ := pod["uid"].(string)
	return uid
}
