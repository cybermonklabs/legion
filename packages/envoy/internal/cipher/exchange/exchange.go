// Package exchange holds what the RFC 8693 token exchange decides with and every identity
// provider shares: the claims a minted Dispatch access token carries — the session that is
// writing, the person it acts for or the service it is, and its list of projects, either the one
// its operator's entry names, the whole list a job's entry names, the one a restricted member's
// machine login asked for, or the ones a trade acting as its person asked for, or none at all
// for a token that only reads — and, beside them, the principal the session name belongs to; the
// entries an operator lists; the rules every entry and every trade share (CheckGrant,
// Claims.Grant, EntryProjects, ResolveSession, ValidSession, ClaimString); and the sentinels a
// refusal wraps.
//
// It verifies no token, reads no configuration, takes no clock and makes no call. Each identity
// provider (internal/cipher/identity/<kind>) decides its own subject tokens with these rules and
// lists its entries as Listings; a Set is built once from every provider's listings and is then
// immutable, and it decides the one trade no provider verifies: a dev box session's machine
// credential, which no entry lists, because the person is the approver the credential already
// records (Machine).
//
// An entry of any provider that names a job list (JobProjects) names no person: the job's own
// identity writes. Its token names that identity as its service — the account subject, the
// workflow as <repository>/<path>@<ref>, the Google account's address — and no person, and
// carries the entry's scope and whole list whatever the trade names: one project under
// ProjectScopeNamed, or under ProjectScopePublic every public project and the private projects
// the list names. A request naming a project the list does not name is refused.
//
// The principal is the caller's standing identity, never the instance asking: the listed account
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
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// Why a subject token is refused. Each stands for one answer an operator can act on: list the
// identity (ErrNoEntry), mint the token in a pod rather than by hand (ErrNoPodBinding), ask under
// the session the entry fixes (ErrSessionMismatch), name a session (ErrSessionRequired), name a
// usable one (ErrSessionInvalid), present a token that carries the claims its kind must
// (ErrClaimMissing), verify the machine's or the person's address with its provider
// (ErrEmailNotVerified), sign in with an account of the configured Workspace domain
// (ErrNotInDomain), present the machine's own identity token beside the person's
// (ErrNoActorToken), mint the machine's token on a Compute Engine instance or unpin its entry
// (ErrNoComputeEngine), run the machine where its entry allows (ErrMachineNotAllowed), of a
// registration, leave an entry that fixes a session alone (ErrSessionFixed), ask for no project
// but those an operator's entry gives (ErrProjectsFixed), ask for no project at all for a token
// that names no person and no job list (ErrProjectsNoPerson), ask for exactly one project for a
// restricted member's machine login (ErrRestrictedOneProject), or say what kind of member a
// machine login's approver is (ErrApproverKindUnknown).
var (
	ErrNoEntry           = errors.New("exchange: no entry lists this identity")
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
	// ErrRestrictedOneProject is a restricted member's machine login asking for no project or for
	// several: its token is held to exactly one (Machine).
	ErrRestrictedOneProject = errors.New("exchange: a restricted member's machine login names exactly one project")
	// ErrApproverKindUnknown is a machine login's decision made with no word on what kind of
	// member its approver is (ApproverUnknown), which no caller of this package means to make.
	ErrApproverKindUnknown = errors.New("exchange: the machine login's approver is of no member kind the decision was told")
)

// ApproverKind is what Dispatch calls a machine login's approver, which the provider asks before
// Machine decides (internal/cipher/oauth): the one fact about a person that changes what that
// person's machine is given. Its zero value is no answer, which Machine refuses, so a caller that
// forgot to ask can never be read as having been told the person is unrestricted.
type ApproverKind int

const (
	// ApproverUnknown is the zero value, which no decision answers.
	ApproverUnknown ApproverKind = iota
	// ApproverUnrestricted is a full member, a person Dispatch holds no row for, and every
	// approver of a broker that checks no project against Dispatch: the machine acts as its
	// person, as it always has.
	ApproverUnrestricted
	// ApproverRestricted is a person an access list admitted from outside the deployment's
	// sign-in domain, who sees only the projects that name them: their machine is held to one of
	// them, named, for at most RestrictedLifetime.
	ApproverRestricted
)

// RestrictedLifetime is the longest a restricted member's machine-login token lives, whatever
// BROKER_ACCESS_TOKEN_SECONDS says when that is longer: 270 seconds. It bounds how long a person
// taken off a project's access list keeps a token for it, with no revoke at all: every trade names
// the project and is checked against Dispatch (projectaccess.Visible), so the last token minted
// for it is minted on a check that read the broker's earlier answer that they see the project, at
// most projectaccess.CacheTTL (30 s) after the removal, and since its lifetime counts from that
// check's return and not from its mint (oauthstore.ExchangeSession.StartAccessLifetime), it ends
// at most 30 + 270 = 300 seconds after the removal; the edge listener's live check closes its
// socket within 25 seconds more, so the person keeps nothing of the project past 325 seconds
// (TestARestrictedTokenEndsWithinTheBound holds the sum). Dispatch's revocation of the member's
// machine logins, which it pays after the removal commits, ends their refresh and their tokens
// sooner, never later.
const RestrictedLifetime = 270 * time.Second

// MaxSessionBytes is the longest session name a request may carry. A session name is written into
// every record the token's writes produce and read back in a dashboard, so it is bounded; 200
// bytes is past any name a Legion session or a person's tool has reason to use.
const MaxSessionBytes = 200

// ProjectScope says what a minted token's list of projects means. Its zero value is no scope at
// all, so a Claims that was not built by this package carries one that every reader refuses:
// the absence of a project must be a decision this package made, never a field nobody filled in.
type ProjectScope int

const (
	// ProjectScopeUnset is the zero value, which no decision answers. A project check handed it
	// refuses the token: a Claims built by anything but this package's constructors has decided
	// nothing, and 'no project named' must not read as 'no oversight'.
	ProjectScopeUnset ProjectScope = iota
	// ProjectScopeNamed is a token held to one project: one an operator's entry mapped, which acts
	// for the entry's person, or as the account itself for an account entry naming no person,
	// Claims.Projects holding the entry's one project or none for an account entry naming none;
	// a job entry's whose list names one project (JobProjects), which names the job as its
	// service and no person, Claims.Projects holding that one project, never none; and a
	// restricted member's machine login's, which acts for that member, Claims.Projects holding
	// the one project its trade asked for (Machine).
	ProjectScopeNamed
	// ProjectScopePerson is a token that acts as its person: Claims.Projects holds the projects
	// its trade asked for, none or more, which the exchange holds to what Dispatch says that
	// person may see before it mints (internal/cipher/oauth). It is what a person's own
	// credential gets, a restricted member's machine login apart, and what a machine whose entry
	// names neither a person nor a job list gets, with no project.
	ProjectScopePerson
	// ProjectScopeNone is a token that may write no project and names none: it holds the OAuth
	// scope oauthwire.ScopeRead and nothing else, so it reads only the routes a resource server
	// lists for that scope, and only on a resource server that serves it. It is what a read
	// entry's pod gets, and no other decision answers it. Its String differs from
	// ProjectScopeUnset's, because 'writes nothing' is a decision this package made and 'never
	// decided' is not one.
	ProjectScopeNone
	// ProjectScopePublic is a job entry's token whose list opens with public (JobProjects): it
	// names the job as its service and no person, it reaches every public project, which no
	// list names, and Claims.Projects holds the private projects the entry lists, 0 to
	// oauthwire.MaxProjects of them, whatever the trade named. It holds no OAuth scope.
	ProjectScopePublic
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
		return "the one project it is held to"
	case ProjectScopePerson:
		return "the projects its person may see that it names"
	case ProjectScopeNone:
		return "no project: reads only"
	case ProjectScopePublic:
		return "every public project and the private projects its entry names"
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
	// which is what a machine whose entry names neither a person nor a job list gets instead of
	// a person, the service a read entry names, the account of an account entry that names no
	// person, which Dispatch records that token's writes under, or a job entry's listed identity,
	// each spelled as the entry spells it — a pod's account subject,
	// system:serviceaccount:<namespace>:<name>, a workflow's <repository>/<path>@<ref>, or a
	// Google account's address. It is empty for every other decision, each of which acts for a
	// person.
	Service string
	// Scope is what Projects means, and is always one of ProjectScopeNamed, ProjectScopePerson,
	// ProjectScopeNone or ProjectScopePublic: every constructor sets it, so a reader never has to
	// read an empty list as any of them. ProjectScopeNone is a read entry's, and is the one scope
	// under which the minted token holds the OAuth scope oauthwire.ScopeRead. A job entry's is
	// its list's (JobProjects.Scope): ProjectScopeNamed or ProjectScopePublic, the latter a job
	// entry's alone.
	Scope ProjectScope
	// Projects is the token's list of projects, sorted, each once: the entry's one project for a
	// ProjectScopeNamed token (none for an account entry naming none) and a restricted member's
	// machine login's one project, for a ProjectScopePerson token the projects its trade asked
	// for, nil for none, for a job entry's token its entry's whole list whatever the trade asked
	// for (one project under ProjectScopeNamed, the private projects under ProjectScopePublic, nil
	// for none), and nil for a ProjectScopeNone token, which names no project. The slice is this
	// Claims' own.
	Projects []string
	// Job is whether a job entry made this decision, which Grant sets from the entry's job list
	// (JobProjects.Listed), so a job's token names the entry's listed identity as its service, no
	// person and the list's projects. Like Principal it is no claim of the minted token: the
	// exchange reads it to refuse a job's trade that asks for read (oauth's decide), since the
	// broker narrows no job's token.
	Job bool
	// LegionStatus is true for the token of a Legion daemon's own entry: an account entry that
	// names a project and fixes the session oauthwire.LegionDaemonSessionPrefix + that project, compared
	// exactly, case included. Such a token is ProjectScopeNamed and holds the OAuth scope
	// oauthwire.ScopeLegionStatus too (oauth.writeProjectScope), which Dispatch reads as leave to
	// write a Legion issue's status. A pod's decision alone sets it (the kubernetes identity
	// provider), and reads the session the entry fixes, never Session: an entry that fixes no
	// session takes Session from the request, which may name any session. So a pod whose request
	// names legion-daemon:<its project> gets no LegionStatus, and neither does an entry that
	// fixes the daemon session of another project or that names no project. No other decision
	// sets it: a job entry, whose Project is empty, a read entry, a workflow, a machine, a
	// person's sign-in and a dev box never do, whatever session they carry.
	LegionStatus bool
	// Principal is who the session name belongs to, and is not a claim of the minted token: it is
	// what the broker records as the name's holder, so a caller that may exchange cannot mint a
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
	// subject must carry. Service holds the same string when the entry names neither a person
	// nor a job list, the account's address when it names a job list, and nothing when it names a
	// person, so a reader that wants the fleet of every machine reads this.
	Fleet string
	// Actor is the machine acting for the person in a delegation (RFC 8693 Section 2.1): the
	// Google account the request's actor token names, as BROKER_VM_SERVICES lists it. It is set
	// by Person alone and empty for every other decision, none of which has an actor. It is a
	// claim of the minted token in the sense that the provider writes it onto the stored session
	// and an introspection answers it (oauth.ClaimMachine), beside the RFC 8693 'act' claim the
	// library builds from the actor token's own subject; this is the account an operator reads.
	Actor string
	// Lifetime is the longest the minted token may live, and is zero for every decision but a
	// restricted member's machine login's, whose token lives at most RestrictedLifetime; zero
	// leaves the broker's own BROKER_ACCESS_TOKEN_SECONDS. Like Principal it is no claim of the
	// minted token: the provider shortens the token's expiry to it, never lengthens it.
	Lifetime time.Duration
}

// Reservation is a session name an entry fixes and the principal it belongs to. The broker holds
// each one for its entry's principal before it makes any exchange, so a name an entry fixes is
// refused to every other caller by the same rule that refuses any held name, whether or not the
// entry's own caller has exchanged yet.
type Reservation struct {
	Session   string
	Principal string
}

// Listing is one identity an operator's entry lists, as its identity provider spells it: the
// principal its tokens hold session names under, the person and the one project the entry gives
// it (both empty for an entry that names no person), the job list it gives it instead (the zero
// JobProjects for an entry that names none), the session the entry fixes ("" for none), and the
// service its machines or its read-only pods are. Where names the entry in a refusal ("account
// entry 1, …"). This package spells no provider's principal: each provider builds its own
// listings, and a Set built from them answers every question that needs no token.
type Listing struct {
	Where, Principal, Person, Project, Session, Service string
	Job                                                 JobProjects
}

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
	// (Claims.Service) and Dispatch records their writes under. An entry that names a project
	// names its person, so no account entry's token that names a project is personless.
	Person string
	// Job, when set, is the job list of an entry that names no person and no project: its pods'
	// tokens name Account as their service and carry the list (BROKER_EXCHANGE_JOB_ACCOUNTS).
	Job JobProjects
	// Session, when set, is the session name every token minted for this account carries, and a
	// request naming any other is refused. It is how a long-running service keeps writing under
	// one name it does not get to choose per request. The name is one of the set's Reservations,
	// held for the account however many of its pods exchange and refused to every other caller.
	// oauthwire.LegionDaemonSessionPrefix + Project makes the entry a Legion daemon's own
	// (Claims.LegionStatus).
	Session string
}

// ReadEntry lists one Kubernetes service account whose pods may read and may write nothing: the
// token they are minted holds the OAuth scope oauthwire.ScopeRead and the project scope
// ProjectScopeNone, names Service and Session, and names no person and no project. Every field is
// required. A resource server accepts such a token only where it asks to, and Dispatch serves it
// on every GET route it classes as a read and nowhere else.
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
// write. Repository, Workflow and Ref are required, and so are Project and Person, or Job in
// their place: nothing is wildcarded by leaving a field out, so an entry that names no ref
// matches no run rather than every one of them. Session is not required.
type WorkflowEntry struct {
	// Repository is <owner>/<name>, the repository the run belongs to.
	Repository string
	// Workflow is the workflow file's path inside the repository, such as
	// .github/workflows/release.yaml, compared exactly.
	Workflow string
	// Ref is the git ref the run is on, such as refs/heads/main, compared exactly.
	Ref string
	// Project is the one project key the minted token names.
	Project string
	// Person is who the run acts for.
	Person string
	// Job, when set, is the job list of an entry that names no person and no project: its runs'
	// tokens name the workflow, <Repository>/<Workflow>@<Ref>, as their service and carry the
	// list (BROKER_EXCHANGE_JOB_WORKFLOWS).
	Job JobProjects
	// Session, when set, is the session name every token minted for this workflow carries, as
	// AccountEntry.Session is for an account.
	Session string
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
// person and names that one project, exactly as an AccountEntry does for a pod, and an entry
// that names neither mints a token that names its service and no person at all. An entry that
// names a job list names neither of them: its token names the account's address as its service
// and carries the list.
type GoogleEntry struct {
	// Account is the service account's email address, the `email` claim of its identity tokens,
	// named exactly: there is no wildcard.
	Account string
	// Service is the service a machine of this account is, which the minted token names when the
	// entry names neither a person nor a job list, and which the launcher credential of every
	// machine of the account records (Claims.Fleet).
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
	Job JobProjects
	// Session, when set, is the session name every token minted for this account carries, as
	// AccountEntry.Session is for an account.
	Session string
}

// Set is the listed identities as the identity providers of one configuration spell them. It is
// immutable once built, so Machine on it is a pure function of its arguments, and every reader of
// a listing reads the configuration the exchange decides against.
type Set struct {
	reservations []Reservation
	// mapped is the person and projects each listed principal's entry gives it, for
	// MappedProjects; a Google entry naming no person and no job list, an account entry naming no
	// person and no job list, and a read entry map their principal to neither, and a job entry to
	// its list and no person.
	mapped map[string]mapping
	// listed is every Listing the set was built from, by principal (Listed).
	listed map[string]Listing
}

// mapping is what an operator's entry gives the tokens of its principal: a person and one
// project or none, a job entry's list and no person, or, for a Google entry naming no person and
// no job list, an account entry naming no person and no job list and a read entry, neither. The
// slice is the set's own and is never handed out.
type mapping struct {
	person   string
	projects []string
}

// mappingOf is what an entry naming person, project and job gives its principal's tokens.
func mappingOf(person, project string, job JobProjects) mapping {
	switch {
	case job.Listed():
		return mapping{projects: job.Projects()}
	case project == "":
		return mapping{person: person}
	}
	return mapping{person: person, projects: []string{project}}
}

// NewSet builds the set from every identity provider's listings, as the identity registry
// composes them: the reservations of the sessions the listings fix, in their order, and each
// principal's person and project, or its job list. Each provider has refused its own entries
// already; what is left for the set is what spans providers: a principal listed twice and a
// session name two listings fix, since one name belongs to one principal, each refused naming
// both entries.
func NewSet(listings []Listing) (*Set, error) {
	set := &Set{mapped: make(map[string]mapping, len(listings)), listed: make(map[string]Listing, len(listings))}
	fixedBy := map[string]string{}
	for _, listing := range listings {
		if other, listed := set.listed[listing.Principal]; listed {
			return nil, fmt.Errorf("exchange: %s: %s lists the same identity, and one identity has one entry", listing.Where, other.Where)
		}
		if listing.Session != "" {
			if other, fixed := fixedBy[listing.Session]; fixed {
				return nil, fmt.Errorf("exchange: %s: %s fixes the same session name, and one name belongs to one identity", listing.Where, other)
			}
			fixedBy[listing.Session] = listing.Where
			set.reservations = append(set.reservations, Reservation{Session: listing.Session, Principal: listing.Principal})
		}
		set.listed[listing.Principal] = listing
		set.mapped[listing.Principal] = mappingOf(listing.Person, listing.Project, listing.Job)
	}
	return set, nil
}

// Reservations are the session names the listings fix, each with the principal it belongs to, in
// the order the listings were given: the providers in rank order, and each provider's entries in
// its own order. The slice is the caller's own, so the set stays immutable.
func (s *Set) Reservations() []Reservation {
	return slices.Clone(s.reservations)
}

// MappedProjects is the person and the projects an operator's entry gives the tokens of
// principal, and false for a principal no entry lists, a person's own among them. A Google entry
// naming no person and no job list, an account entry naming no person and no job list, and a
// read entry, answer neither, and true. A job entry answers no person and its whole list: a named
// list's one project, or a public list's private projects, beside which its token reaches every
// public project. It is how a reader holding no token — a release for a pod or a machine — reads
// the list that pod's or that machine's token would carry. The slice is the caller's own.
func (s *Set) MappedProjects(principal string) (person string, projects []string, ok bool) {
	entry, listed := s.mapped[principal]
	if !listed {
		return "", nil, false
	}
	return entry.person, slices.Clone(entry.projects), true
}

// Listed is the listing of principal, and false for a principal no listing names, a person's own
// among them. It is how a reader holding no token reads what an operator's entry says of an
// identity: the service a machine's account is listed for, and the person its entry names.
func (s *Set) Listed(principal string) (Listing, bool) {
	listing, ok := s.listed[principal]
	return listing, ok
}

// ProjectsForAccount is the projects this configuration gives a token minted for one account,
// and false for an account no listing names. An account that opens with "system:serviceaccount:"
// is read as a Kubernetes subject, any other as a Google account address, so a caller passes the
// string its own configuration holds and spells no principal. It reads the listings the
// kubernetes and google providers decide from, so a grant made from this answer and the token of
// that account cannot disagree. A workflow is no account and is answered false.
func (s *Set) ProjectsForAccount(account string) ([]string, bool) {
	principal := GooglePrincipal(account)
	if strings.HasPrefix(account, oauthwire.AccountSubjectPrefix) {
		principal = AccountPrincipal(account)
	}
	_, projects, ok := s.MappedProjects(principal)
	return projects, ok
}

// EntryProjects is the list a token an operator's entry maps carries: that entry's one project,
// or none for an account entry that names none. A request may name the entry's project or name
// nothing; one naming anything else is refused, since the entry is what chose the project and a
// trade may neither widen nor change it.
func EntryProjects(identity, project string, asked []string) ([]string, error) {
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

// jobProjects is the list a job entry's token carries: the entry's whole list, whatever the
// request names, since the entry is the deployment's grant to the job and a trade may neither
// widen nor narrow it. A request may name no project, or keys of the list; one naming any other
// key is refused, a public project's under a public list included, since the broker cannot tell
// a public project's key from one no project holds. The slice is the caller's own.
func jobProjects(identity string, job JobProjects, asked []string) ([]string, error) {
	listed := job.Projects()
	for _, key := range asked {
		if !slices.Contains(listed, key) {
			return nil, fmt.Errorf("%w: %s's entry gives its token the projects %s, and the request named %s, which that list does not name",
				ErrProjectsFixed, identity, job, key)
		}
	}
	return listed, nil
}

// Grant sets in c what an operator's entry naming person and project, or the job list job, gives
// its token: for a person, Person, ProjectScopeNamed and the entry's one project or none
// (EntryProjects); for an account entry naming neither a person nor a job list, service as
// Service, ProjectScopeNamed and no project, since its token acts as the account itself; for a
// job list, service as Service, Job, the list's scope and its whole list (jobProjects). identity
// is the caller as a refusal of the request's projects names it, and service the identity a
// token naming no person names, which each identity provider spells its own way: a pod's account
// subject, a workflow's <repository>/<path>@<ref>, a Google account's address. It is the one
// place an entry's grant becomes a token's claims, so every provider's entries grant alike. It
// refuses what those two refuse, and c is then the caller's to drop.
func (c *Claims) Grant(identity, service, person, project string, job JobProjects, asked []string) error {
	var err error
	if job.Listed() {
		c.Job, c.Service, c.Scope = true, service, job.Scope()
		c.Projects, err = jobProjects(identity, job, asked)
	} else {
		c.Person, c.Scope = person, ProjectScopeNamed
		if person == "" {
			// Only an account entry names neither a person nor a job list here (CheckGrant), and
			// it names no project either: the account is the service its token names in
			// place of a person, and what Dispatch records its writes under.
			c.Service = service
		}
		c.Projects, err = EntryProjects(identity, project, asked)
	}
	return err
}

// CheckGrant refuses what an entry's grant cannot be, deciding first whether it names the job
// list job. An entry that names one is refused a person or a project beside it, since an entry
// names a person and its project, or a job list, and never both; the list itself needs no check,
// since only ParseJobProjects builds one. Every other entry but an account entry naming no
// project is refused an empty person, which would mint a token answerable to nobody, so a
// workflow entry that names neither a person nor a job list is refused here. Its project is
// required too, except of an account entry (account set), which may name a person and no project
// — the shared public-session account's entry, minting named tokens that name no project (a pod
// account many people launch must never act as one person's own token) — or name neither, when
// its tokens act as the account itself (Grant). An account entry that names a project names its
// person, so no account entry's token that names a project is personless. A person or a project
// that is blank without being empty is refused everywhere, and so is a project that is no project
// key: every token the entry mints names it, the exchange stores and reads that list through
// oauthwire.ParseProjects, which takes only project keys, so an entry naming anything else would
// boot and then fail every trade. Every entry, of either kind, is refused a fixed session name no
// request could name back.
func CheckGrant(where, person, project string, job JobProjects, session string, account bool) error {
	if job.Listed() {
		if person != "" || project != "" {
			return fmt.Errorf("exchange: %s: the entry names the job list %s beside a person or a project (person %q, project %q); an entry names a person and its project, or a job list, and never both", where, job, person, project)
		}
	} else {
		if !(account && project == "") && strings.TrimSpace(project) == "" {
			return fmt.Errorf("exchange: %s: the project is required, since the minted token names one project; an entry names a person and its project, or a job list", where)
		}
		if project != "" {
			if err := oauthwire.ValidProjects([]string{project}); err != nil {
				return fmt.Errorf("exchange: %s: the entry's project %v", where, err)
			}
		}
		switch {
		case account && person == "" && project != "":
			return fmt.Errorf("exchange: %s: an account entry names no person only when it names no project either: an entry that names a project names its person, so no account entry's token that names a project is personless", where)
		case !(account && person == "") && strings.TrimSpace(person) == "":
			return fmt.Errorf("exchange: %s: the person is required, since a token acts for someone unless its entry names a job list", where)
		}
	}
	if session != "" && !ValidSession(session) {
		return fmt.Errorf("exchange: %s: %s", where, SessionRule)
	}
	return nil
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
// A full member's token acts as its person, and says so: its scope is ProjectScopePerson, and it
// carries the projects the trade asked for. A machine credential is a person's own identity on
// their own machine, as their browser cookie and their personal token are: the person reaches
// whichever projects they may reach, and the list says which of them this session works on, which
// the exchange holds to what Dispatch says that person may see before it mints. Naming the scope
// is what keeps a reader of an introspection answer from mistaking an empty list for a token no
// project check applies to. kind is ApproverUnrestricted for that person, and for one Dispatch
// holds no row for.
//
// A restricted member's token is held to one project instead (kind ApproverRestricted): an
// outside person's agents act in the one project they work on and in nothing of the person's
// own. Its scope is ProjectScopeNamed, which a resource server gives no scope of a person's own
// (Dispatch's minting of personal tokens and its approvals among them), and it carries exactly
// the one project the trade asked for, which the exchange then holds to what Dispatch says that
// person sees; a trade asking for none or for several is refused, ErrRestrictedOneProject. It
// lives at most RestrictedLifetime.
//
// A kind nobody decided (ApproverUnknown) is refused, ErrApproverKindUnknown, never read as
// either.
func (s *Set) Machine(credential, approver, session string, projects []string, kind ApproverKind) (Claims, error) {
	if strings.TrimSpace(credential) == "" {
		return Claims{}, fmt.Errorf("%w: the subject token names no machine credential id, so it is not a machine login's token", ErrClaimMissing)
	}
	if strings.TrimSpace(approver) == "" {
		return Claims{}, fmt.Errorf("%w: the machine credential records no approver to act for", ErrClaimMissing)
	}
	name, err := ResolveSession("", session, approver)
	if err != nil {
		return Claims{}, err
	}
	claims := Claims{Session: name, Person: approver, Projects: slices.Clone(projects), Principal: PersonPrincipal(approver)}
	switch kind {
	case ApproverUnrestricted:
		claims.Scope = ProjectScopePerson
	case ApproverRestricted:
		if len(projects) != 1 {
			return Claims{}, fmt.Errorf("%w: %s is a restricted member, who sees only the projects that name them, and the trade asked for %d; set AGENT_SECRETS_PROJECTS to the one project this session works on",
				ErrRestrictedOneProject, approver, len(projects))
		}
		claims.Scope, claims.Lifetime = ProjectScopeNamed, RestrictedLifetime
	default:
		return Claims{}, fmt.Errorf("%w: %s's machine login was decided with member kind %d", ErrApproverKindUnknown, approver, int(kind))
	}
	return claims, nil
}

// The spellings of a principal, each led by its kind so that principals of two kinds never spell
// the same string. Each identity provider spells its own (internal/cipher/identity/<kind>); the
// person's is here, because a dev box's machine credential, which no provider verifies, and a
// person's sign-in both hold their names under it.
//
// A broker grant follows a session name under one principal too: enroll.Service.Create writes one
// of these into enrollments.carry_principal, and the next enrollment of that name under that
// principal takes what the last one held. The name a session writes its Dispatch tokens under and
// the name its grant follows are one name, so one holder has to spell the same either way, which
// it does only while there is one definition of each spelling. A pod's enrollment is the one
// exception, whose grant follows a pair rather than its account alone (PodLaunchPrincipal), while
// its name is held under the account. A pod of an account the broker pairs with that account
// (enroll.SessionBinder.PairedWith) takes what the other account's pods of the name hold only
// under the same login account, the pair LaunchedBy spells from the paired account's principal.

// AccountPrincipal is a Kubernetes ServiceAccount's spelling: the account a pod's own projected
// token proves.
func AccountPrincipal(account string) string { return "account:" + account }

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

// SessionRule is the one sentence that says what a session name may be, so an entry refused at
// build time and a request refused at exchange time are refused in the same words, and the cap it
// quotes is the cap ValidSession applies.
var SessionRule = fmt.Sprintf("a session name is printable, carries no whitespace, '*' or '>', and is at most %d bytes", MaxSessionBytes)

// ResolveSession answers the session claim for an entry whose fixed session is fixed and a request
// that named named, where identity names who is asking. An entry that fixes a session answers it
// and refuses a request that names another, rather than quietly putting the fixed one in its
// place; an entry that fixes none has nothing to fall back on, so the request must name a usable
// session.
//
// A refused name is described and never repeated: it comes from the request, so its length and
// its bytes are whatever the caller sent.
func ResolveSession(fixed, named, identity string) (string, error) {
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
		return "", fmt.Errorf("%w: %s named a session of %d bytes; %s", ErrSessionInvalid, identity, len(named), SessionRule)
	}
	return named, nil
}

// ValidSession reports whether name is usable as a session claim: UTF-8, printable, with no
// whitespace, no '*' or '>', and at most MaxSessionBytes. A '*' or a '>' would make the session's
// relay subject (bus.DeliverSubject) a NATS wildcard over every session's, which a relay socket of
// that session would subscribe. A '.' is taken: a person's sign-in trades under a name that holds
// the person's address (cipherclient.SessionFor), and the NATS grant and the relay socket hold a
// session to one subject token themselves (natsgrant.SessionToken). A pod-session registration of
// a name it refuses is refused before it is written (Registration), and a release of registrations
// (oauthstore.Store.ReleasePodSessions) drops such a name as one that matches nothing rather than
// refusing it.
func ValidSession(name string) bool {
	if name == "" || len(name) > MaxSessionBytes || !utf8.ValidString(name) || strings.ContainsAny(name, "*>") {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// ClaimString reads one claim the decision needs as a non-empty string. A claim that is absent,
// null, or of another JSON type is the same fault to a caller: the subject token is not the kind
// of token this decision is for.
func ClaimString(token map[string]any, name string) (string, error) {
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
