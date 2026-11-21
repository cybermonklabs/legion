// packages/envoy/internal/cipher/requests/machine_state.go
// Package requests is the broker's state machine for agent_secret credential requests. A request
// row freezes, at creation, the enrollment, the resource set with each name's decision and the
// secret it reads, the allowed approver, the policy version and the lifetime its request object
// signed, else the 12-hour default (defaultLifetime): a grant lasts that long, or until its session
// ends when the request names none and every name is tagged default-grant=session.
// Each name's airlock fingerprint is written with the decision rather than with the row: at
// creation for a request the policy answers at once, and again when an approval grants one that
// waited for a person (decidedUnder), so a grant approved after a rules roll is released under
// the entry it was decided under.
// Its only transitions are pending -> granted | denied | cancelled | expired; each is an UPDATE
// guarded by state='pending' inside one transaction that also writes the audit row, so a duplicate
// or late decision changes nothing. A request that needs approval also freezes an append-only
// credential-request record (internal/cipher/record): the requester's signed request object plus
// the broker's decision fields. The record is decided by a person signed in to the broker's own
// approval page (internal/cipher/approval), whose session names the login that decides it: its
// approver's, or anyone's for a record whose approver is record.AnyoneApprover. Every release of a
// grant it produced re-verifies the whole chain — record hash, requester signature, one approval
// by a login the record allows — so a row written by anyone but the broker releases nothing.
//
// One column of a request row is not frozen: its enrollment_id, and the same holds for a grant's
// and a withheld name's. A session that enrols on a box it has not run on before takes what the
// enrollment it is moving off holds — its grants, its requests and the names its operator withheld
// from it — because an approval a person gives hours later, and the grant that comes of it, belong
// to the session rather than to the box that asked (enroll.Create's adoption,
// dispatch://INFRA-1167). So every read and every transition here is keyed by the enrollment the
// row itself names now, never by the one the append-only credential_requests record froze: that is
// what gets a moved session its own grant back, lets it cancel its own request, and still refuses
// it another session's. moved_session_test.go drives each of those surfaces across a move.
package requests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

var (
	ErrNotYours = errors.New("this request or grant belongs to another session")
	// ErrNotOwner is the refusal of a caller whose owner did not make the request it names
	// (OwnerAndState, EnrollmentOwner): the read route's and Cancel's. It is an ErrNotYours to
	// errors.Is, so every caller that refuses on ErrNotYours refuses on it, in words that name the
	// owner rule rather than one session.
	ErrNotOwner    error = notOwnerError{}
	ErrNotApprover       = errors.New("only the grant's approver or the person its session acts for may revoke it")
	ErrTerminal          = errors.New("request is already decided")
	// ErrExpired is ApplyDecision's refusal for a record that expired undecided: the sweeper
	// expired its request, or its pending_expires_at has passed before the sweeper got to it.
	ErrExpired = errors.New("request expired before its approver acted on it")
	// ErrNotExpired is Dismiss's refusal for a record nobody has decided whose deadline has not
	// passed: it may still be decided, so there is nothing to dismiss yet.
	ErrNotExpired   = errors.New("request has not expired; it can still be decided")
	ErrGrantNotLive = errors.New("grant is expired, revoked, or its session ended")
	// ErrCommitUncertain is a decision whose COMMIT failed: its connection was lost while it
	// committed, or its backend was ended, so whether it applied only a read of what it wrote
	// can say (LogJudged's, for the judge). It wraps the COMMIT's own error.
	ErrCommitUncertain = errors.New("the decision's COMMIT failed, so whether it applied only a read of what it wrote can say")
	// ErrGrantChainInvalid: re-verifying a grant's whole approval chain (record hash, requester
	// signature, one approval by the record's approver) failed — a row written by anyone but the
	// broker releases nothing.
	ErrGrantChainInvalid = errors.New("this grant's approval chain no longer verifies")
	ErrSecretNotInStore  = errors.New("secret is not in the secrets store")
	ErrMixedApprovers    = errors.New("the requested secrets need different approvers; request them separately")
	// ErrGrantRequiresLifetime: a request names a secret whose max-grant tag requires a signed
	// lifetime no longer than that tag. Its returned error names the secret and the limit.
	ErrGrantRequiresLifetime = errors.New("a requested secret requires a shorter grant lifetime")
	// ErrMintFailed: a type secret's minter could not make a token from the stored value, such as
	// a Google login that needs a new sign-in or a GitHub App GitHub refuses. Its returned error
	// carries the minter's own message, which never holds the stored value or a token.
	ErrMintFailed = errors.New("the broker could not mint a token from this secret")
	// ErrNarrowingNotApplicable: a request or a release asked to narrow a GitHub App token, and
	// the names it covers hold no github-app secret, so nothing it releases could be narrowed.
	// Create refuses the request before anybody is asked to approve it, and Values refuses the
	// release of a grant; either way nothing is released rather than every value whole.
	ErrNarrowingNotApplicable = errors.New("this grant holds no github-app secret to narrow")
	// ErrNarrowingOwnerNotListed: a request or a release asked to narrow a GitHub App token to an
	// account that a github-app secret of it does not list in its github-owners tag. Create
	// refuses the request and Values the release; nothing is released, and no minter runs, since
	// the caller's input, not the App, is at fault.
	ErrNarrowingOwnerNotListed = errors.New("the narrowing names an account a github-app secret of this grant does not serve")
	// ErrNarrowingBehindAirlock: a request or a release asked to narrow a GitHub App token while
	// the airlock holds back one of its names. Nothing is released, and no minter runs. The
	// broker cannot apply such a narrowing and say so truthfully: the names behind the airlock
	// are spent through the proxy, which mints its own values once per grant per pod and would go
	// on swapping in the owner-wide token. Its returned error names the name the airlock holds.
	ErrNarrowingBehindAirlock = errors.New("this grant holds a name behind the airlock, which takes no narrowing")
	// ErrRequestPending means this session already has a person deciding one of the requested
	// names. Its returned error names that pending request.
	ErrRequestPending = errors.New("a request for one or more of these secrets is already pending")
	// ErrSessionIDInvalid: the request's own session_id override (requests.session_id — an
	// unsigned claim any enrolled process may send, never verified against a signature) is too
	// long or carries a whitespace or control character, refused before it is ever stored or
	// forwarded to Envoy's wake (LEGION-587).
	ErrSessionIDInvalid = errors.New("session id must be at most 128 characters with no whitespace or control character")
)

// notOwnerError is ErrNotOwner: its own words, and ErrNotYours to errors.Is.
type notOwnerError struct{}

func (notOwnerError) Error() string { return "this request belongs to another owner" }

func (notOwnerError) Is(target error) bool { return target == ErrNotYours }

// refusals is every sentinel this package refuses a request or a grant with: the ones above and
// the two of airlock.go. A sentinel added to this package belongs in it, which is why it sits
// beside them rather than in a caller that reads them.
var refusals = []error{
	ErrNotYours, ErrNotOwner, ErrNotApprover, ErrTerminal, ErrExpired, ErrNotExpired, ErrGrantNotLive, ErrGrantChainInvalid,
	ErrSecretNotInStore, ErrMixedApprovers, ErrGrantRequiresLifetime, ErrMintFailed,
	ErrNarrowingNotApplicable, ErrNarrowingOwnerNotListed, ErrNarrowingBehindAirlock,
	ErrRequestPending, ErrSessionIDInvalid, ErrAirlockNoNames, ErrAirlockValueShape,
}

// IsRefusal reports whether err is one of this package's own refusals, as against a read that
// did not answer at all. The broker's proxy port answers a refusal of a grant in the error's own
// words, since they are the broker's answer about that grant and no retry clears them, and
// answers every other error in words of its own, because a Postgres connect error carries the
// database's user, its name and its address and because a read that failed is no proof that a
// grant ended (proxy's grantRefusal, the one caller).
func IsRefusal(err error) bool {
	for _, refusal := range refusals {
		if errors.Is(err, refusal) {
			return true
		}
	}
	return false
}

// maxSessionIDLength bounds the request body's session_id override; see ErrSessionIDInvalid.
const maxSessionIDLength = 128

// validSessionID reports whether sessionID is acceptable to store: empty (unset) or at most
// maxSessionIDLength runes with no whitespace or control character.
func validSessionID(sessionID string) bool {
	if utf8.RuneCountInString(sessionID) > maxSessionIDLength {
		return false
	}
	for _, r := range sessionID {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// jtiRetentionMargin is how long past a request object's expiry its jti is remembered, mirroring
// proof.Verifier's own retention margin.
const jtiRetentionMargin = time.Minute

// DefaultMissRereads bounds the policy rereads one enrollment's unknown-name requests trigger:
// a session inventing names costs at most Burst DescribeSecret calls, refilled one per Every.
// The budget is per process, so two broker pods make two of it for the same enrollment. That is
// left as it is because this bounds load rather than authority - a reread only moves a pod
// toward the secret store's own truth, and grants nothing - so the cost of sharing it across
// pods, a store read's worth of state in Postgres on every miss, buys nothing an operator needs.
var DefaultMissRereads = ratelimit.Limit{Every: 10 * time.Second, Burst: 10}

// SecretDecision is how the policy decided one name of a request.
type SecretDecision struct {
	// The secret's name.
	Name string `json:"name"`
	// "automatic", "approval" or "deny".
	Decision string `json:"decision"`
	Source   string `json:"-"`
	// AirlockDigest is the fingerprint of the name's airlock entry as the policy held it when
	// the request was decided, "" for a name the rules do not name. The request records it, and
	// a release ends the grant once it differs from the one the policy holds then.
	AirlockDigest string `json:"-"`
}

// Request is one agent_secret request as Create and Get answer it. Approver is the approver its
// credential-request record names (a person's canonical login, or record.AnyoneApprover), read
// from the record rather than the current policy, since the record is what the approver's list
// shows; it is nil when the request has no record, because the policy decided it at once.
// LifetimeSeconds is the lifetime its grant lasts: the one its signed request object named, else
// the 12-hour default; nil when it named none and every name is tagged default-grant=session.
type Request struct {
	ID              string           `json:"request_id"`
	State           string           `json:"state"`
	GrantID         *string          `json:"grant_id"`
	RecordID        *string          `json:"record_id"`
	Approver        *string          `json:"approver"`
	Secrets         []SecretDecision `json:"secrets"`
	LifetimeSeconds *int             `json:"lifetime_seconds"`
	DecidedAt       *time.Time       `json:"decided_at"`
	DecidedBy       *string          `json:"decided_by"`
	Detail          *string          `json:"detail"`
	Coalesced       bool             `json:"coalesced,omitempty"`
	// JudgeDeciding is whether the model judge holds the request now: it is pending, and its
	// judge_until lies ahead. The broker's answer then names no approval page, since no person
	// may decide it until the judge has given it up.
	JudgeDeciding bool `json:"-"`
}

// Decision is ApplyDecision's result: the request's new state and, when granted, the new grant's
// id (empty when the record was denied, or approved but the requesting enrollment had died).
type Decision struct {
	RequestID string
	State     string
	GrantID   string
	// Detail is what the decision recorded beside the request's new state, empty when it
	// recorded none. An approval ends denied with one of two: the requesting enrollment was no
	// longer live, or the secret's requesters tag no longer lists the session
	// ("<NAME>: requester-not-listed"). The approval page answers the approver with it, so the
	// person who pressed approve reads why the broker denied it instead.
	Detail string
}

// Minter makes one release of a type secret from its stored value and the policy's facts about it
// (secrets.GitHubApp.Mint, secrets.GoogleLogin.Mint): the token, and when it stops working. Its
// error never holds the stored value or a token.
type Minter func(ctx context.Context, req secrets.MintRequest) (secrets.Minted, error)

// GiveBack tries to undo, at its upstream, one minted value of a type that a release minted for the
// secret name and then handed to nobody (secrets.GitHubApp.GiveBack): a release that failed after
// the mint, or a session's release of a name the airlock holds back, which mints the value only to
// learn its slots. It runs to a bound of its own whatever ctx does, logs what the upstream did not
// confirm, and answers nothing, since nothing the release answers depends on it.
type GiveBack func(ctx context.Context, name, value string)

// PendingRequest is one credential request Create has just written pending, as it hands it to
// Machine.Judge: what the broker's model judge reads of a request, and nothing else.
//
// It is declared here, in the package that writes it, and the judge takes it as it stands
// (judge.Runner.Consider): the import runs one way, since Machine.Judge is a func field the
// broker's main wires up and never a call this package makes into that one, so the judge names
// this machine's type and this package names no judge.
type PendingRequest struct {
	// RecordID is the credential-request record a decision is written against, and
	// RequestID the request row it belongs to.
	RecordID, RequestID string
	// EnrollmentID is the enrollment that asked, which the policy is re-read for, and
	// SessionID the Envoy session that enrollment recorded, empty for one that names none.
	EnrollmentID, SessionID string
	// Secrets are the names the request asks for, in the request's own name order, and
	// Decided those of them that need a decision: the judge keys the judge decides. The rest
	// are keys the session gets at once, which the grant releases beside them.
	Secrets, Decided []string
	// LifetimeSeconds is the grant's lifetime as the record holds it: the one the requester
	// signed, else the broker's default for its names (defaultLifetime), and nil for a grant
	// that lasts until its session ends. An unbounded grant is what the judge's seconds are
	// there to bound, so the judge decides none.
	LifetimeSeconds *int
	// Reason is the requester's own signed reason, which names the Dispatch issue, and
	// Approver the record's approver: the secret's owner, or record.AnyoneApprover for a
	// shared one. The judge decides in that approver's place and names it in its log line.
	Reason, Approver string
}

type Machine struct {
	Store      *store.Store
	Policy     *policy.Current
	Secrets    secrets.Reader
	PendingTTL time.Duration
	// Audience is BROKER_PUBLIC_URL: the aud every request object's signature is checked against.
	Audience string
	Skew     time.Duration
	// Replay records a request object's jti (the proof_jtis table), answering false when it was
	// already seen. Wired to enroll.Service.Replay in production.
	Replay func(ctx context.Context, jti string, expires time.Time) (fresh bool, err error)
	// Chain re-verifies a grant's whole approval chain on every release (VerifyChain): the same
	// record.ChainVerifier machinery enroll.Service.AuthenticateLauncher uses for launcher
	// credentials, built by NewChainVerifier against agent_secret records instead.
	Chain *record.ChainVerifier
	// MissRereads bounds, per enrollment, the single-name policy rereads Create makes for names the
	// live policy does not serve (rereadMissing); nil means DefaultMissRereads.
	MissRereads *ratelimit.Limit
	// NarrowingCheckBudget bounds the whole check Create makes of a request's GitHub App
	// narrowing before it asks a person (narrowing_check.go), and NarrowingCheckMint one mint of
	// that check and that mint's revoke, but for the give-back of a token whose answer the check
	// could not use, which runs to a detached bound of its own; zero means
	// DefaultNarrowingCheckBudget and DefaultNarrowingCheckMint.
	NarrowingCheckBudget time.Duration
	NarrowingCheckMint   time.Duration
	// NarrowingChecks bounds, per enrollment, the checks of that narrowing that reach GitHub;
	// nil means DefaultNarrowingChecks. An enrollment past the bound has the GitHub part of its
	// check skipped and its request goes to its approver, as it did before the check existed.
	NarrowingChecks *ratelimit.Limit
	// ServicesByAccount are the registered services' accounts (BROKER_SERVICES): for each account,
	// the service a pod whose projected token proved it runs as that account proves, when its
	// launcher's service is that one (requester).
	ServicesByAccount map[string]string
	// PodOperators is BROKER_POD_OPERATORS: each Kubernetes ServiceAccount subject that the
	// deployment maps to a person, answering that person's login. A pod enrollment's operator
	// is the person its pod acts for only while this map still names that person for the
	// account the enrollment recorded (unmappedPersonPod). While it does, the pod asks as that
	// person (requester), and its requests are that person's to read and cancel (owner). The map
	// is read on every request, decision, release, read of a request and cancel, as
	// ServicesByAccount is. So when an entry is removed, or names another person, an enrolled pod
	// of that account acts as its account's own at its next call. From then on, its requests are
	// its own enrollment's alone.
	PodOperators map[string]string
	// VMServicesByAccount is BROKER_VM_SERVICES, keyed by a Google service account's email: the
	// service a machine whose Google identity token the exchange took proves, when its launcher
	// credential's service is that one (requester). It is read on every call, so an account
	// dropped from the setting stops its machines' service secrets at their next one.
	VMServicesByAccount map[string]string
	// VMPersonsByAccount is each fleet's person, keyed by its Google service account's email
	// (BROKER_VM_EXCHANGE_ACCOUNTS' person, an entry that names none left out): whose Dispatch
	// Inbox lists a request one of its machines' own sessions makes for a shared human-tier
	// secret (responsiblePerson). It grants nothing: a machine's session acts for no person.
	VMPersonsByAccount map[string]string
	// Assignees names a Dispatch issue's assignee, whose Inbox lists a request a Legion pod makes
	// for a shared human-tier secret when its claim names that issue (claimAssignee); nil reads
	// nothing, and such a request is listed for everyone. cmd/cipherd wires the judge's read of
	// Dispatch where BROKER_JUDGE_DISPATCH_URL is set.
	Assignees Assignees
	// Minters maps each policy type (policy.Types) to its Minter. A release never returns the
	// stored value of a type secret.
	Minters map[string]Minter
	// GiveBacks maps a policy type whose minted value the upstream can take back to its GiveBack,
	// which a release calls for each such value it read and hands to nobody (giveBack, heldBack,
	// release.go). A type with none, and a Machine with no map, leaves such a value to lapse.
	GiveBacks map[string]GiveBack
	// Airlock is the airlock's rules as the broker read them at boot
	// (BROKER_AIRLOCK_RULES_FILE), and the zero value for a deployment with no rules file. A
	// granted name the rules name is released as placeholders rather than as its value
	// (Values), and the proxy port puts the real value back into the requests the entry allows.
	Airlock proxyrules.Rules
	// AirlockProxyURL is BROKER_AIRLOCK_PROXY_URL, the URL a command reaches that port at, and
	// AirlockCACertificate the PEM certificate of its certificate authority
	// (BROKER_AIRLOCK_CA_CERT_FILE, read at boot). Every release that holds a name back carries
	// both, since a command that cannot reach the proxy or cannot trust it has nothing.
	AirlockProxyURL      string
	AirlockCACertificate string
	// Judge, when set, is handed every request Create has just written pending, once that
	// transaction has committed and the request's grant-log line is out: the broker's model
	// judge, which may approve such a request in its approver's place (cmd/cipherd wires it to
	// judge.Runner.Consider, and nothing else ever sets it). A nil field is no judge at all,
	// which is every test and every deployment that configures none; a human-tier request
	// then waits for the person who decides it today, exactly as before.
	//
	// It is called for a request this Create wrote pending and never for one it coalesced
	// onto a pending request already there, so one record is never judged twice. Nothing a
	// judge does belongs on the requester's path, so the hook must return at once: Consider
	// starts a goroutine of its own and does.
	Judge func(PendingRequest)
	// Projects asks Dispatch whether a project is private, whether a person may see it and what
	// kind of member a person is (projectaccess.Client, BROKER_PROJECT_ACCESS_URL), the client
	// enroll.Service.Projects is. Create, every release, decision and airlock connection read the
	// member kind of the person a session was made for (restrictedOperator), and a restricted
	// member's session is denied every secret; a project is read only for a request, release,
	// decision or airlock connection holding a project-tagged secret (projectFacts). Both are read
	// before any transaction. nil on a broker with no such client, which asks nothing and serves
	// no tagged secret (policy.Loader.ProjectChecks).
	Projects projectaccess.Access
	// Exchange is the broker's token exchange configuration, read for the project an operator's
	// entry maps a pod's or a machine's account to (exchange.Set.MappedProjects), which is the
	// project such a credential names. nil names none.
	Exchange *exchange.Set
	// SignInOff is BROKER_SIGNIN=off: the broker signs nobody in and serves no approval page, so
	// no person can ever decide a request. Create then denies at once every name whose answer
	// would be an approval, policy.ReasonSignInOff, and writes no pending request, no record and
	// nothing for Judge (config.Load refuses a judge beside it), and the sweep denies what a
	// broker with sign-in on left or leaves pending (denyUndecidable); every name the policy
	// grants at once is granted as ever.
	SignInOff bool
	// missRereads is MissRereads' per-enrollment buckets, built on first use (missRereadLimiter).
	missRereads     *ratelimit.Keyed
	missRereadsOnce sync.Once
	// channelsMissingLogged throttles ChannelsMissingMessage to one line per project, and
	// rowsUnansweredLogged rowUnansweredMessage and fallbackUnreadMessage to one per listed row and
	// fallbackCutMessage to one per reader, per projectaccess.CacheTTL: each machine's own, so one
	// machine's lines never silence another's.
	channelsMissingLogged logThrottle
	rowsUnansweredLogged  logThrottle
	reasonsUnreadLogged   logThrottle
	// narrowingChecks is NarrowingChecks' per-enrollment buckets, built on first use
	// (narrowingCheckLimiter).
	narrowingChecks     *ratelimit.Keyed
	narrowingChecksOnce sync.Once
	// givingBack counts the give-backs running in the background (giveBack, release.go), which a
	// test waits on before it reads what they did.
	givingBack sync.WaitGroup
	// leftOut is, by record id, each pending record PendingForApprover has logged as left out of
	// a list because it does not verify for this broker, with when to forget it: the later of the
	// record's expiry and leftOutRetention after the last list that left it out. It is logged once
	// rather than on every poll (leaveOut).
	leftOutMu sync.Mutex
	leftOut   map[string]time.Time
	// testAdoptionHook, when set, runs in applyDecisionAttempt between the read that locks the
	// requesting enrollment and the read that locks the request. It exists only so a test can
	// deterministically move the request to another enrollment in that window, which is where an
	// adoption's commit lands; no production caller sets it.
	testAdoptionHook func()
	// testRevokeHook, when set, runs in RevokeByApprover between its person rule
	// (hideUnseenGrant) and its transaction's read of the grant. It exists only so a test can move
	// the grant to another enrollment in that window, where an adoption's commit can land; no
	// production caller sets it.
	testRevokeHook func()
	// testCommitCut, when set, runs right after a decision's transaction has committed, and an
	// error it returns is what the decision answers: a COMMIT the server applied whose answer
	// the network then lost. testHoldCommit, when set, runs with every row of the decision
	// written and before its COMMIT, and an error it returns rolls the decision back: a COMMIT
	// still in flight. testLandedRead, when set, is what LogJudged's read of whether an
	// approval's own row is there fails with. The two that run inside a transaction are handed
	// the process id of the backend that holds it, so a test that ends a backend ends that one
	// alone: every test of every package shares one database (storetest), and a test that ended
	// any other backend would cut another test's transaction. They exist only so a test can cut a
	// judge's approval after it applied, hold it uncommitted, and fail that read; no production
	// caller sets them.
	testCommitCut  func() error
	testHoldCommit func(backend uint32) error
	testLandedRead func(backend uint32) error
}

// missRereadLimiter is the per-enrollment buckets MissRereads names, built on the first miss.
func (m *Machine) missRereadLimiter() *ratelimit.Keyed {
	m.missRereadsOnce.Do(func() {
		m.missRereads = ratelimit.NewKeyed(limitOr(m.MissRereads, DefaultMissRereads))
	})
	return m.missRereads
}

// limitOr is what a limiter field of this Machine bounds its load at: the limit the field names,
// or fallback, the package's default, for the nil field every caller that sets none leaves. Each
// limiter keeps its own sync.Once and its own buckets; this is the one rule they share
// (missRereadLimiter above, narrowingCheckLimiter in narrowing_check.go).
func limitOr(limit *ratelimit.Limit, fallback ratelimit.Limit) ratelimit.Limit {
	if limit != nil {
		return *limit
	}
	return fallback
}

// NewChainVerifier builds the record.ChainVerifier VerifyChain uses, scoped to agent_secret
// records. rules are the airlock's, the ones the machine itself holds (Machine.Airlock): they
// reach the verifier as record.ChainVerifier.JudgeSeconds, so a grant the model judge approved
// re-verifies against the same bound the judge decided under. Zero rules give the judge nothing,
// which refuses every such approval.
func NewChainVerifier(st *store.Store, audience string, skew time.Duration, rules proxyrules.Rules) *record.ChainVerifier {
	chain := st.ChainVerifier(record.KindAgentSecret, audience, skew)
	chain.JudgeSeconds = rules.JudgeSeconds
	// The keys of a judged record's request that needed a decision, off the request's own
	// rows; every other key is one its session got at once, which the release's own re-check
	// (stillAllowed) holds to the live policy.
	chain.Decided = func(ctx context.Context, recordID string) (func(string) bool, error) {
		var requestID string
		if err := st.Pool.QueryRow(ctx, `select id from requests where record_id=$1`, recordID).Scan(&requestID); err != nil {
			return nil, err
		}
		requested, err := requestedSecrets(ctx, st.Pool, requestID)
		if err != nil {
			return nil, err
		}
		return neededADecision(requested), nil
	}
	return chain
}

type enrollmentRow struct {
	ID, Kind, Thumbprint string
	// Operator is the person the enrollment was made for: the operator of the launcher credential
	// that enrolled it. It is nil for a machine's own session, and for a pod whose login carried
	// no person. A pod acts for this person only while BROKER_POD_OPERATORS still maps its
	// account to that person (requester). Service is the service of the launcher credential that
	// enrolled it, nil for a person's machine. Subject is a pod's verified service account or the
	// credential subject a machine's session recorded, nil for every other session. Host is that
	// credential's host.
	Operator, Service, Subject, Host *string
	RuntimeID, Slot                  string
	// SessionID is the Envoy session this enrollment recorded, empty for one that names none.
	// It is read with the rest of the row, rather than looked up when a decision is logged, so
	// the grant-log line a committed transaction writes names the session it decided for
	// without a second query (grantLogEvent).
	SessionID string
	// Projects are the Dispatch projects a session's enrollment named (enrollments.projects),
	// empty for none and for every pod but a person's pod, which names them as a session does.
	Projects []string
	// Claim is what a pod's launcher stated the pod runs for (enrollments.claim), "" for none:
	// for the Legion daemon, the role claim token whose issue's assignee is asked for a shared
	// secret's request (claimAssignee). The live-enrollment read (enrollment) fills it.
	Claim string
	// Restricted is whether Dispatch called the person this enrollment was made for a restricted
	// member when the caller asked, before any transaction (restrictedOperator): Create and every
	// release ask, and requester hands it to the policy, which denies such a session every
	// secret. It is false wherever nobody asked.
	Restricted bool
}

// requester is enrollment e as the policy sees it, read through q: the person it acts for (none
// for a machine's own session), its service, and the names that person withheld from it
// (RevokeByApprover's withhold). Its service is its launcher credential's only
// when the account the enrollment recorded still proves it: `BROKER_SERVICES` for a pod's
// verified service account, `BROKER_VM_SERVICES` for the Google account a machine's credential
// subject names (exchange.ReadSubject). A machine login's service name is the machine's own
// claim, which anyone signed in to Dispatch may approve, so the account is what proves the
// service. Any other session has none.
// A pod acts for its enrollment's operator only while `BROKER_POD_OPERATORS` (PodOperators)
// still maps the account the enrollment recorded to that same person (unmappedPersonPod).
// Otherwise the pod acts for no person: it is its account's own, and its service is read as
// above. A pod whose login carried no person acts for no person.
// Every other session acts for its enrollment's operator. The enrollment row keeps the person it
// was made for, which the approval record, the grant log and the grant list's selection read;
// mayRevoke, the person rule's exemption and the grant list's operator read whom the enrollment
// acts for now (operatorOf), as this does.
// It is read on every request, decision and release, so a change to one of these settings
// changes what an enrolled pod or machine gets at its next call, not at its lease's end. A
// caller that decides
// for the session reads it after locking the session's row, which RevokeByApprover holds while it
// withholds, so a withhold either committed before the read or waits for the caller.
// Create's check of a narrowing is the one caller that reads it on the pool under no lock
// (checkNarrowing): it decides only whether that request would reach a person and so whether to
// ask GitHub about the narrowing, never what the request is given, so a withhold that commits
// beside it changes nothing but whether one check runs.
// The withheld names are read by the enrollment's own id, and an adoption carries a session's
// withheld_secrets rows to the enrollment it moves to, so these are the names withheld from the
// session rather than from a box it has left.
// A session whose person the caller found a restricted member (enrollmentRow.Restricted) is
// marked so, and the policy denies it every secret (policy.ReasonRestrictedMember). That person is
// the one the enrollment was made for (restrictedOperator), so a person's pod the setting no
// longer maps is marked too; a service's pod and a machine's own session, made for no person, are
// never marked.
func (m *Machine) requester(ctx context.Context, q querier, e enrollmentRow, facts projectFacts) (policy.Requester, error) {
	rows, err := q.Query(ctx, `select name from withheld_secrets where enrollment_id=$1`, e.ID)
	if err != nil {
		return policy.Requester{}, err
	}
	withheld, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return policy.Requester{}, err
	}
	var service string
	if e.Subject != nil && e.Service != nil {
		switch e.Kind {
		case "pod":
			if bound, ok := m.ServicesByAccount[*e.Subject]; ok && bound == *e.Service {
				service = *e.Service
			}
		case "session":
			if read := exchange.ReadSubject(*e.Subject); read.Kind == exchange.SubjectMachine {
				if bound, ok := m.VMServicesByAccount[read.Account]; ok && bound == *e.Service {
					service = *e.Service
				}
			}
		}
	}
	return facts.requester(policy.Requester{Operator: m.actingPerson(e), Service: service, Withheld: withheld, Restricted: e.Restricted}), nil
}

// actingPerson is the person the session e acts for: its operator, and no one for a pod whose
// account BROKER_POD_OPERATORS no longer maps (unmappedPersonPod), a service's pod and a
// machine's own session. requester decides by that person. restrictedOperator does not: it asks
// Dispatch about the person e was made for, whom the setting does not change.
func (m *Machine) actingPerson(e enrollmentRow) string {
	if m.unmappedPersonPod(e) {
		return ""
	}
	return deref(e.Operator)
}

// restrictedOperator reports whether Dispatch calls the person e was made for (its operator, the
// person of the launcher credential that enrolled it) a restricted member
// (projectaccess.Restricted), asked through m.Projects on no transaction, as the rule
// internal/cipher/projectaccess states for every caller is: Create asks it before it reads a
// live grant to reuse, every release before it re-checks the grant, and every airlock proxy
// connection before it carries anything (AirlockConnection), since the proxy holds a grant's
// values past the release that read them. A service's pod and a machine's own session, made for
// no person, ask nothing, and neither does a machine with no client of Dispatch.
//
// The person is the operator the enrollment recorded, not the one requester reads
// (actingPerson). A person's pod whose account BROKER_POD_OPERATORS drops, or maps to another
// person, acts for no person from then on, which decides whose owned secrets it may have; it is
// still the pod that person's login enrolled, so it is still asked about as that person. Were it
// asked about the person it acts for, a restricted member's pod would ask nothing once the setting
// changed, and be given every shared agent-tier secret again.
//
// It is the second half of the rule that a restricted member's agents get no secret, the first
// being that such a person's machine login enrolls nothing (enroll.Service.checkMember). This
// half reaches the sessions that rule never saw: one a broker with no client of Dispatch
// enrolled, and one whose person Dispatch called something else when it enrolled. A failure to
// ask, of any kind, is a *projectaccess.MemberKindUncheckable, which Create, every release, every
// airlock connection and every approval return as it is: a check that cannot decide refuses, so
// while Dispatch cannot be asked about a person nothing is decided, released or carried for that
// person's sessions, and each asks again.
//
// An approval of a request still pending from before the broker asked asks too, before its
// transaction (applyDecisionAttempt, for the enrollment its read before the transaction found),
// and records a restricted member's request as the policy's denial, restricted-member, so no
// grant is written for it; and so does the model judge's read of the policy (JudgePolicy), which
// then answers deny before any model is asked.
func (m *Machine) restrictedOperator(ctx context.Context, e enrollmentRow) (bool, error) {
	person := deref(e.Operator)
	if m.Projects == nil || person == "" {
		return false, nil
	}
	return projectaccess.Restricted(ctx, m.Projects, person)
}

// Create verifies the request object (record.VerifyRequestObject, jti replay through the Replay
// seam), requires iss to be this enrollment's own key and no login_hint (session requests never
// name their own approver — that's the policy's job) and a reason naming the issue keys of no more
// than record.MaxReasonProjects projects (record.CheckReasonProjects), rereads each name the live
// policy does not serve (rereadMissing), and hands back a live grant for exactly these names when
// one may be reused (reuseLiveGrant). Otherwise it decides and writes the request in one
// transaction: it takes the advisory locks that serialize pending requests for each requested name
// in one enrollment (lockPendingNames), then locks the enrollment live (lockLiveEnrollment), so no
// request or grant lands on an enrollment that ended after Create first read it (such an
// enrollment is pgx.ErrNoRows, as one that had ended before), and no withhold
// (RevokeByApprover, which takes the same row) lands between the read of what is withheld from
// the session and the write. It then evaluates the policy for the session and writes a granted or
// denied request, with a granted one's grant, or a pending request and its credential-request
// record. A second pending request that overlaps one of its names is refused, so an untrusted
// session cannot flood an approver's Inbox by varying --ttl. A pending request whose approver is
// record.AnyoneApprover records the person responsible for the agent that made it
// (responsiblePerson), whose Inbox alone lists it, or nobody, and then everyone's does; for a
// Legion pod that person is its claim's issue's assignee, read from Dispatch before the
// transaction opens (claimAssignee).
//
// Before that rule, on every ask that writes a request, pending or decided at once, it ends each
// pending request of the enrollment for one of the requested names whose record is signed for
// another broker address (signedForAnotherBroker), whatever its age, and keeps every other one
// (endSignedForAnotherBroker); a request past its deadline is neither, since every decision of it
// answers ErrExpired and the sweep expires it (pendingRequests). Such a request was asked of a
// broker of another BROKER_PUBLIC_URL that shares this database, or of this broker before its
// public URL changed, and its session now asks here: no person and no judge here can decide it,
// and left pending it would hold every overlapping ask to ErrRequestPending until it expired, or
// wait beside the request kept or the one decided at once until then. The broker cancels it in this
// transaction, once the policy's evaluation has answered, through store.EndPendingRequests
// (brokerMovedEnd, BrokerMovedDetail). For a request that needs a decision the rule then applies
// to the oldest request it kept: Create coalesces onto it when the names and the lifetime join,
// otherwise refuses with ErrRequestPending naming it, and with none kept writes the new request
// pending. A request decided at once, granted or denied, is written as it is with nothing
// pending. The ends commit on each of those paths, the refusal's included, since a refusal writes
// no row and the transaction then holds the ends alone, and each end is logged once that commit
// has succeeded (logBrokerMoved, RequestEndedBrokerMovedMessage, naming the request the session
// is referred to), so a commit that fails logs none.
//
// An ask a live grant answers (reuseLiveGrant) writes no request, and Create hands that grant
// back before the transaction above. endBesideReusedGrant ends the same requests all the same: a
// read in no transaction tells whether any of them is signed for another broker address, and only
// when one is does a short transaction of its own, under the advisory locks of the asked names and
// the enrollment's lock, end them, commit and then log each end naming the reused grant's request.
// With none it opens no transaction and writes nothing beyond the jti Replay already wrote, so
// such an ask waits on no writer of the enrollment row. An error of that read or of that
// transaction is the ask's. An ask Create refuses before it writes a request ends nothing: an
// invalid or replayed request object, an unknown name
// (policy.ErrUnknownSecret, which the evaluation answers), a name whose value is pending, a
// lifetime over a name's max-grant, a narrowing refusal or ErrMixedApprovers (the evaluation's
// too). Each of them returns before any end runs, since the main transaction ends requests only
// once the evaluation has answered and the reuse path only once a live grant has: no request of
// that ask exists for the line to name, and the session's next valid ask ends that request.
//
// An end takes the request's row lock after the advisory locks, which every Create takes in
// sorted name order, and after the enrollment's lock for share. applyDecisionAttempt, which
// decides such a request on the broker it was signed for, takes the enrollment's lock for share
// and then the row's, Cancel the row's alone, and an enrollment's end (enroll's endEnrollment)
// updates the enrollment before it ends the enrollment's pending requests, so none of them waits
// on a lock Create holds while Create waits on the row, and none of them deadlocks with it. Two
// Creates of the enrollment for different names can both find one request that holds a name of
// each; they end what they find in one order (pendingRequests), so the second waits on the
// first's row and holds none the first waits on.
// Two sweeps fall outside that order: each takes no enrollment lock and ends many pending
// requests in one statement, in whatever order that statement reaches them. One is the sweep's
// expiry (expirePending, store.RequestsPastDeadline). The other is the sweep's denyUndecidable
// (store.RequestsByIDs) on a broker of another address that runs with BROKER_SIGNIN=off: it ends
// the pending requests that verify for that broker, the very requests this broker ends as signed
// for another broker. An end here takes the rows one at a time in (created_at, id) order
// (endSignedForAnotherBroker), so when such a sweep ends two stale requests of one session while
// that session's ask ends them, the two transactions can lock the rows in opposite orders, and
// Postgres ends one of them with a deadlock error: the sweep's, which its next tick runs again, or
// the ask's, which answers that error and which the session's next ask runs again. The
// transaction Postgres ends rolls back whole and the other ends each row once, so no row is left
// in a wrong state. For the expiry the window is narrow: pendingRequests reads only requests
// whose deadline had not passed when the transaction began (requestBeforeDeadline, over
// Postgres's now()), so the expiry and an end here reach the same row only when its deadline
// passes between that start and the sweep's own now, and two of a session's requests must both
// do so for the locks to cross.
// An end that finds the request decided meanwhile ends nothing and leaves that decision as it is.
//
// A request it writes pending that the judge holds (judgeHolds) is handed to Machine.Judge once
// the transaction has committed; one a person decides, and one it coalesced onto a pending
// request, is not, so the model judge sees each record it holds once.
//
// narrowing is the GitHub App narrowing the caller will release this request's grant with, nil
// when it asks for none. A later step of Create checks it before the request reaches a person, so
// a narrowing the broker would refuse costs nobody an approval.
func (m *Machine) Create(ctx context.Context, enrollmentID, compactRequest, sessionID string, narrowing *secrets.GitHubNarrowing) (Request, error) {
	if !validSessionID(sessionID) {
		return Request{}, ErrSessionIDInvalid
	}
	enr, err := m.enrollment(ctx, enrollmentID)
	if err != nil {
		return Request{}, err
	}
	obj, err := record.VerifyRequestObject(compactRequest, m.Audience, m.Skew, time.Now())
	if err != nil {
		return Request{}, err
	}
	// The names the request asks for, read the moment its JWS has opened, so every refusal of
	// it from here on names them (answer.Recorder.NoteSecrets). A request object's details are
	// all agent secrets or one machine login (record.VerifyRequestObject), and a machine login's
	// identifier is a host, not a secret, so it names none; the check below refuses it.
	var names []string
	for _, d := range obj.Details {
		if d.Type == record.KindAgentSecret {
			names = append(names, d.Identifier)
		}
	}
	answer.FromContext(ctx).NoteSecrets(names...)
	if obj.LoginHint != "" {
		return Request{}, fmt.Errorf("%w: login_hint is only for machine login requests", record.ErrRequestInvalid)
	}
	if obj.Thumbprint != enr.Thumbprint {
		return Request{}, fmt.Errorf("%w: iss is not this enrollment's own key", record.ErrRequestInvalid)
	}
	if len(obj.Details) == 0 || obj.Details[0].Type != record.KindAgentSecret {
		return Request{}, fmt.Errorf("%w: this endpoint accepts only agent_secret authorization_details", record.ErrRequestInvalid)
	}
	if err := record.CheckReasonProjects(obj.Reason); err != nil {
		return Request{}, err
	}
	fresh, err := m.Replay(ctx, obj.JTI, obj.Expires.Add(m.Skew+jtiRetentionMargin))
	if err != nil {
		return Request{}, err
	}
	if !fresh {
		return Request{}, fmt.Errorf("%w: replayed jti", record.ErrRequestInvalid)
	}
	return m.createChecked(ctx, enr, enrollmentID, sessionID, obj, names, narrowing)
}

// createChecked is Create past the rules a new request object alone is held to — its issuer, no
// login_hint, agent_secret details, a reason naming at most record.MaxReasonProjects projects, a
// fresh jti — and writes the request as Create's doc says. Those rules stay in Create, where a
// request is new, and createChecked holds the rest, so a test can write through it the request a
// broker from before one of them would have stored.
func (m *Machine) createChecked(ctx context.Context, enr enrollmentRow, enrollmentID, sessionID string, obj record.RequestObject, names []string, narrowing *secrets.GitHubNarrowing) (Request, error) {
	// Whether the session's person is a restricted member, asked here, on no transaction and
	// before a live grant is looked for, so every read below — the reuse's re-check, the judge's
	// lifetime, the narrowing check and the transaction's own evaluation — denies such a session
	// every name (restrictedOperator). An answer that could not be had refuses the ask whole.
	var err error
	if enr.Restricted, err = m.restrictedOperator(ctx, enr); err != nil {
		return Request{}, err
	}
	set, err := m.rereadMissing(ctx, enrollmentID, names, m.Policy.Get())
	if err != nil {
		return Request{}, err
	}
	// A secret `agent-secrets secret create` made and has not yet filled is refused as such, never
	// as unknown, which a caller takes as final: no request row, record or approver for it.
	for _, name := range names {
		if m.Policy.PendingValue(set, name) {
			return Request{}, fmt.Errorf("%s %w", name, policy.ErrPendingValue)
		}
	}
	lifetime := defaultLifetime(set, names, obj.LifetimeSeconds)
	// A key the judge decides grants at most its own judge seconds, but only when the judge
	// decides the request: a request naming no lifetime gets the shortest judge seconds of the
	// keys the judge decides, and one that asks for longer is refused here, before anyone is
	// asked, as a max-grant tag refuses it. A key the session gets at once beside them is no key
	// the judge decides, so its own rules entry bounds nothing. The bound comes before the
	// max-grant tags, which then read the lifetime the judge would grant. Whether the judge will
	// hold the request is read now off the same evaluation the transaction below makes; a request
	// a person decides — a judge key beside a key with no judge entry, or any request on a broker
	// that runs no judge — keeps the ordinary lifetime rules.
	// What Dispatch holds of the requested names' projects, read here on the pool, before the
	// transaction below takes its locks, so no call to Dispatch holds one; a request holding no
	// tagged name asks it nothing. Every evaluation of the request reads them: the judge's
	// pre-evaluation below, the reuse of a live grant, the narrowing check, the responsible
	// person's and the transaction's own.
	facts, err := m.projectFacts(ctx, enr, names, set)
	if err != nil {
		return Request{}, err
	}
	judgeBounded := false
	if m.Judge != nil && m.namesAJudgeKey(set, names) {
		requester, err := m.requester(ctx, m.Store.Pool, enr, facts)
		if err != nil {
			return Request{}, err
		}
		pre, err := m.evaluate(set, names, requester, obj.Reason)
		if err != nil {
			return Request{}, err
		}
		if pre.state == "pending" && m.judgeDecides(set, pre.decisions) {
			if lifetime, err = m.judgeLifetime(set, decidedNames(pre.decisions), obj.LifetimeSeconds, lifetime); err != nil {
				return Request{}, err
			}
			judgeBounded = true
		}
	}
	for _, name := range names {
		if limit, over := overMaxGrant(set, name, lifetime); over {
			return Request{}, fmt.Errorf("%w: %s allows at most %s; ask with agent-secrets --ttl %s", ErrGrantRequiresLifetime, name, limit, limit)
		}
	}
	sorted := sortedCopy(names)
	if existing, ok, err := m.reuseLiveGrant(ctx, enr, sorted, lifetime, set, facts); err != nil {
		return Request{}, err
	} else if ok {
		if err := m.endBesideReusedGrant(ctx, enrollmentID, sorted, existing.ID); err != nil {
			return Request{}, err
		}
		return existing, nil
	}
	if narrowing != nil {
		// The verdict on the narrowing, before a row is written and before anybody is asked
		// (checkNarrowing, narrowing_check.go): a refusal returns here, so the approval a
		// narrowing the broker cannot serve would have spent is never asked for.
		if err := m.checkNarrowing(ctx, enr, names, set, facts, obj.Reason, narrowing); err != nil {
			return Request{}, err
		}
	}
	assignee := m.claimAssignee(ctx, enr, set, names, obj.Reason, facts)
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return Request{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockPendingNames(ctx, tx, enrollmentID, sorted); err != nil {
		return Request{}, err
	}
	if err := lockLiveEnrollment(ctx, tx, enrollmentID); err != nil {
		return Request{}, err
	}
	requester, err := m.requester(ctx, tx, enr, facts)
	if err != nil {
		return Request{}, err
	}
	e, err := m.evaluate(set, names, requester, obj.Reason)
	if err != nil {
		return Request{}, err
	}
	// Who decides is the keys' setting, read now and kept on the row: the judge holds a request
	// whose every key needing a decision is a judge key, on a broker that runs a judge; every
	// other pending request is a person's from the start. judgeBounded is the same answer read
	// before the transaction, which bounded the lifetime by the judge seconds: a request the
	// withheld names moved into the judge's class in between was never bounded, so it is its
	// person's.
	judgeHolds := e.state == "pending" && m.Judge != nil && m.judgeDecides(set, e.decisions) && judgeBounded
	// The ends run here, whatever the evaluation decided the request to, pending, granted or
	// denied, and only once it has answered, so an ask it refuses ends nothing. ended is every
	// request this call ended because its record is signed for another broker address, logged
	// once the transaction that ended it has committed, whichever way Create then answers the
	// session; kept is the oldest overlapping request it kept, one not signed for another broker
	// address, the zero row when there is none, which the one-pending-request rule applies to for
	// a request that needs a decision.
	kept, ended, err := m.endSignedForAnotherBroker(ctx, tx, enrollmentID, sorted)
	if err != nil {
		return Request{}, err
	}
	if e.state == "pending" && kept.ID != "" {
		if slices.Equal(kept.Names, sorted) && (kept.joins(lifetime) || (obj.LifetimeSeconds == nil && kept.Lifetime == nil)) {
			if err := tx.Commit(ctx); err != nil {
				return Request{}, err
			}
			logBrokerMoved(ended, kept.ID, enrollmentID)
			existing, err := m.Get(ctx, kept.ID)
			existing.Coalesced = true
			return existing, err
		}
		refused := fmt.Errorf("%w: request %s is still pending", ErrRequestPending, kept.ID)
		if len(ended) == 0 {
			return Request{}, refused
		}
		// The requests it ended stay ended though the ask is refused. The transaction holds
		// nothing but those ends, since a refusal writes no row, so it commits them first.
		if err := tx.Commit(ctx); err != nil {
			return Request{}, err
		}
		logBrokerMoved(ended, kept.ID, enrollmentID)
		return Request{}, refused
	}
	r := newRequest{
		id: uuid.NewString(), enrollmentID: enrollmentID, reason: obj.Reason, state: e.state,
		approver: e.approver, rulesVersion: set.Version, sessionID: sessionID, lifetime: lifetime, decisions: e.decisions,
		requester: requester, denials: e.denials, unexplained: e.unexplained, projectMissing: e.projectMissing,
	}
	if e.state == "pending" && e.approver == record.AnyoneApprover {
		r.notify = m.responsiblePerson(enr, requester, assignee)
	}
	// Both branches end the same way — commit, then the one grant-log line for the request.created
	// audit row insertRequest wrote — so only how req is produced differs between them.
	var req Request
	if e.state != "pending" {
		req = Request{ID: r.id, State: r.state, Secrets: r.decisions, LifetimeSeconds: r.lifetime, Detail: nullable(r.decisionDetail())}
		if err := m.insertRequest(ctx, tx, r); err != nil {
			return Request{}, err
		}
		if r.state == "granted" {
			grantID, err := insertGrant(ctx, tx, r.id, enrollmentID, "", r.lifetime)
			if err != nil {
				return Request{}, err
			}
			req.GrantID = &grantID
		}
	} else {
		r.pendingExpiresAt = time.Now().Add(m.PendingTTL)
		r.judgeHolds = judgeHolds
		pending, err := m.createPending(ctx, tx, enr, r, obj)
		if err != nil {
			return Request{}, err
		}
		pending.JudgeDeciding = judgeHolds
		req = pending
	}
	if err := tx.Commit(ctx); err != nil {
		return Request{}, err
	}
	logBrokerMoved(ended, r.id, enrollmentID)
	store.LogGrantEvent(r.createdRequestLogEvent(enr))
	// The model judge, for a request this call has just written pending and held for it. It is
	// called here, past the commit and past the grant-log line, so the judge's own reads find the
	// request and the record that are now there and nothing it does runs inside the transaction.
	// The coalescing branch above returns before this point, so a request that joined a pending
	// one is never handed over a second time and one record is never judged twice. A request a
	// person decides is never handed to the judge at all.
	if judgeHolds {
		m.Judge(PendingRequest{
			RecordID: deref(req.RecordID), RequestID: r.id, EnrollmentID: enrollmentID,
			SessionID: enr.SessionID, Secrets: r.names(), Decided: decidedNames(r.decisions), LifetimeSeconds: r.lifetime,
			Reason: r.reason, Approver: deref(req.Approver),
		})
	}
	return req, nil
}

// BrokerMovedDetail is the detail of a pending request Create ends because its record is signed
// for another broker address (signedForAnotherBroker): the request's decision_detail, which its
// session reads back as the request's own Detail, its record's cancelled event's detail, and the
// reason of its request.cancelled audit row. It says why nobody decided the request, so a session
// that reads it back, on either broker, learns that its ask on this one took its place.
const BrokerMovedDetail = "the broker moved: the request was signed for another broker address, so no person and no judge on this broker could decide it, and its session's new request replaces it"

// brokerMovedEnd is how Create ends such a request: cancelled by the broker, which records itself
// as having decided it, as Cancel and an enrollment's end record their own actor, with
// BrokerMovedDetail as its detail and, in its audit row, under "reason", the key an enrollment's
// end gives an ended request's (enroll.endEnrollment). One value is handed to
// store.EndPendingRequests and to endedRequestLogEvent alike, so the row and its grant-log line
// cannot drift; nothing writes it.
var brokerMovedEnd = store.RequestEnd{
	State: "cancelled", Actor: "broker", DecidedBy: new("broker"), Detail: BrokerMovedDetail,
	AuditDetail: map[string]any{"reason": BrokerMovedDetail},
}

// endSignedForAnotherBroker ends in tx each pending request of enrollmentID for one of sorted
// whose record is signed for another broker address (signedForAnotherBroker), whatever its age
// beside a request of this broker, with brokerMovedEnd, and keeps every other one; a request past
// its deadline is not read at all (pendingRequests). It answers the oldest request it kept, the
// zero row when it kept none, which Create's one-pending-request rule applies to, and the rows
// its ends returned, which its caller logs once tx has committed (logBrokerMoved). It
// reads the overlapping requests once, oldest first (pendingRequests), and ends any only after
// that read has closed its rows. One read sees them all, since tx holds the advisory lock of every
// name in sorted (lockPendingNames), so no other Create of the enrollment writes a request for one
// of them while tx runs. An end returns no row when the broker the request was signed for decided
// it between the read and the end: that decision holds the row's lock, the end waits on it, and
// once the decision commits the request is no longer pending, so the end leaves the decision as
// it is, and that request is neither kept nor ended.
func (m *Machine) endSignedForAnotherBroker(ctx context.Context, tx pgx.Tx, enrollmentID string, sorted []string) (pendingRequestRow, []store.EndedRequest, error) {
	pending, err := pendingRequests(ctx, tx, enrollmentID, sorted)
	if err != nil {
		return pendingRequestRow{}, nil, err
	}
	var kept pendingRequestRow
	var ended []store.EndedRequest
	for _, p := range pending {
		if !m.signedForAnotherBroker(p) {
			if kept.ID == "" {
				kept = p
			}
			continue
		}
		rows, err := store.EndPendingRequests(ctx, tx, store.RequestByID, p.ID, brokerMovedEnd)
		if err != nil {
			return pendingRequestRow{}, nil, err
		}
		ended = append(ended, rows...)
	}
	return kept, ended, nil
}

// endBesideReusedGrant is endSignedForAnotherBroker for an ask a live grant answered
// (reuseLiveGrant), which writes no request, so Create returns before its own transaction: it
// ends each pending request of enrollmentID for one of sorted whose record is signed for another
// broker address, commits, and then logs each end naming reused, the reused grant's request, as
// the request the session is referred to (logBrokerMoved).
// It reads those requests first straight on the pool, in no transaction (pendingRequests), and
// when none is signed for another broker address (signedForAnotherBroker) it answers nil: no
// transaction, no lock and no log line. Most asks a live grant answers find none, and a
// transaction would cost each of them: the enrollment's lock for share assigns it a transaction
// id, so it would commit durably a second time, beside the jti Replay writes, and wait behind any
// writer of the enrollment row. Only when that read finds such a request does it open a short
// transaction of its own, take the advisory locks of sorted (lockPendingNames) and then the
// enrollment's row lock for share, in the order Create's transaction takes them, and end what
// endSignedForAnotherBroker, reading again under those locks, finds; that second read decides
// what is ended. The read without locks can miss only a request that reaches the enrollment after
// it, such as one a hand-over (enroll's handOver) moves there, and the session's next ask ends
// that request, as it would had the request arrived just after the locked read. A request the
// broker it was signed for decides between the two reads is no longer pending at the locked read,
// which ends nothing for it.
// The enrollment's lock is what keeps an enrollment's end (enroll's endEnrollment), which updates
// the enrollment and then ends all its pending requests in one statement, in whatever order that
// statement reaches them, from holding one of these requests while this transaction holds
// another: one of the two waits for the other's commit before it takes a row. It does not order
// this transaction against the sweep's expiry (expirePending) or, on a broker of another address
// with BROKER_SIGNIN=off, the sweep's denyUndecidable, which take no enrollment lock and end
// several requests in one statement too: one of those and this transaction can lock two of the
// session's requests in opposite orders (the expiry only in the narrow window Create's doc
// names), and Postgres then ends one of the two with a deadlock error; the next tick or the next
// ask runs it again, and no row is left in a wrong state. Unlike lockLiveEnrollment it does not
// ask that the enrollment be live, since a live grant has already answered the ask. An error of it
// is Create's, so a read, a transaction or a commit that fails answers the ask with that error and
// logs no end.
func (m *Machine) endBesideReusedGrant(ctx context.Context, enrollmentID string, sorted []string, reused string) error {
	pending, err := pendingRequests(ctx, m.Store.Pool, enrollmentID, sorted)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(pending, m.signedForAnotherBroker) {
		return nil
	}
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockPendingNames(ctx, tx, enrollmentID, sorted); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `select 1 from enrollments where id=$1 for share`, enrollmentID); err != nil {
		return err
	}
	_, ended, err := m.endSignedForAnotherBroker(ctx, tx, enrollmentID, sorted)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	logBrokerMoved(ended, reused, enrollmentID)
	return nil
}

// logBrokerMoved logs each request endSignedForAnotherBroker ended, once the transaction that
// ended them has committed: the grant-log line of the request.cancelled audit row its end wrote,
// then RequestEndedBrokerMovedMessage naming newRequest, the request Create refers the session
// to. That is the request Create wrote, granted, denied or pending; the oldest kept request of
// this broker it coalesced onto or refused the ask with ErrRequestPending for; or, for an ask a
// live grant answered, the reused grant's request (endBesideReusedGrant). Each caller calls it
// straight after the commit, before anything that can fail, so an end that committed is logged
// and one that did not never is.
func logBrokerMoved(ended []store.EndedRequest, newRequest, enrollmentID string) {
	for _, r := range ended {
		store.LogGrantEvent(endedRequestLogEvent(brokerMovedEnd, r.EnrollmentID, r.ID))
		slog.Info(RequestEndedBrokerMovedMessage, "request", r.ID, "record_id", deref(r.RecordID), "new_request", newRequest, "enrollment", enrollmentID)
	}
}

// decidedNames is the names of decisions that needed a decision — the policy answered approval —
// in their own order: the keys a request the judge holds has it decide, where every other key is
// one the session gets at once.
func decidedNames(decisions []SecretDecision) []string {
	var names []string
	for _, d := range decisions {
		if d.Decision == policy.Approval {
			names = append(names, d.Name)
		}
	}
	return names
}

// rereadMissing rereads each requested name the live set does not serve (the spec's miss path: a
// secret created seconds ago is not in the listing yet), bounded per enrollment by MissRereads,
// and answers the set to evaluate against. A name no secret can carry (policy.Current.CheckName:
// a free-text identifier, or one past Secrets Manager's name limit) stays unknown before it costs
// a token or a line. A failed reread logs LoadFailedMessage and the name stays unknown — the
// caller sees UNKNOWN_SECRET exactly as before the miss path existed. A reread that fails once ctx
// has ended (the caller went away, or its deadline passed) is the request's failure, not Secrets
// Manager's: it logs nothing and answers ctx's error, so a LoadFailedMessage line always reports
// the store's failure and never a client that left. It runs before Create's transaction, so no
// Secrets Manager call holds a row lock.
func (m *Machine) rereadMissing(ctx context.Context, enrollmentID string, names []string, set *policy.Set) (*policy.Set, error) {
	for _, name := range names {
		if _, ok := set.Secrets[name]; ok {
			continue
		}
		if m.Policy.CheckName(name) != nil {
			continue
		}
		if !m.missRereadLimiter().Allow(enrollmentID) {
			return set, nil
		}
		if _, err := m.Policy.RefreshOne(ctx, name); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			slog.Error(policy.LoadFailedMessage, "name", name, "error", err)
			continue
		}
		set = m.Policy.Get()
	}
	return set, nil
}

// DefaultGrantLifetime is how long a grant lasts when its request names no lifetime, unless every
// name it grants carries default-grant=session (policy.TagDefaultGrant), or one of them is human
// tier (DefaultHumanGrantLifetime).
const DefaultGrantLifetime = 12 * time.Hour

// DefaultHumanGrantLifetime is how long a grant lasts when its request names no lifetime and any
// name it grants is human tier. A person approves such a request by the action its reason names,
// and the broker enforces only the duration, so a long default hands every later action of that
// session the same approval. Fifteen minutes is the window in which the broker releases and reuses
// that credential, not a count of actions: a release inside it is still unasked, and a value
// already in a process is beyond the broker. A session that needs longer says so with --ttl, which
// the approver reads on the card.
const DefaultHumanGrantLifetime = 15 * time.Minute

// defaultLifetime is the lifetime a request's grant lasts: the one it signed, else nil (until its
// session ends) when every name opts out with default-grant=session, else
// DefaultHumanGrantLifetime when any name is human tier, else DefaultGrantLifetime. A name the set
// does not serve (a malformed tag among them) keeps the 12-hour default, so it never yields a
// session-long grant; its tier is unknown, so it never shortens one either.
func defaultLifetime(set *policy.Set, names []string, signed *int) *int {
	if signed != nil {
		return signed
	}
	fallback, optedOut := DefaultGrantLifetime, len(names) > 0
	for _, name := range names {
		secret, ok := set.Secrets[name]
		if !ok || !secret.SessionDefault {
			optedOut = false
		}
		if ok && secret.Tier == policy.TierHuman {
			fallback = DefaultHumanGrantLifetime
		}
	}
	if optedOut {
		return nil
	}
	seconds := int(fallback / time.Second)
	return &seconds
}

// overMaxGrant reports whether a request signing lifetime (nil: until its session ends) asks for
// longer than name's max-grant tag allows, and that bound as a --ttl the CLI accepts. A name with
// no max-grant, or one the set does not serve (left to evaluate, which refuses it as unknown), is
// never over.
func overMaxGrant(set *policy.Set, name string, lifetime *int) (string, bool) {
	maxGrant := set.Secrets[name].MaxGrant
	if maxGrant == 0 || (lifetime != nil && time.Duration(*lifetime)*time.Second <= maxGrant) {
		return "", false
	}
	return formatGrantDuration(maxGrant), true
}

// formatGrantDuration is a max-grant as a --ttl the CLI accepts: whole seconds only
// (ttlSeconds), so a tag such as 1.5s is suggested as 1s, the longest whole lifetime it allows.
func formatGrantDuration(duration time.Duration) string {
	duration = duration.Truncate(time.Second)
	switch {
	case duration%time.Hour == 0:
		return strconv.FormatInt(int64(duration/time.Hour), 10) + "h"
	case duration%time.Minute == 0:
		return strconv.FormatInt(int64(duration/time.Minute), 10) + "m"
	default:
		return duration.String()
	}
}

// evaluation is one pass of the policy over a request's names. approver is set when a name needs
// approval, and then names the one approver every such name has. denials are the denied names
// a reason was named for, in the request's own name order: the request row records them
// as its decision_detail (newRequest.decisionDetail), so a refused session is told which secret
// refused it and why instead of only that something did. A name the policy would have put to an
// approver is a denial here, policy.ReasonSignInOff, on a broker that signs nobody in, and
// otherwise policy.ReasonNoReason when the request carries no reason.
// unexplained is set when some name was denied with no reason to name, which decisionDetail has
// its own fixed sentence for.
type evaluation struct {
	decisions   []SecretDecision
	denials     []denial
	state       string // granted, denied or pending
	approver    string
	unexplained bool
	// projectMissing are the names denied policy.ReasonProjectMissing, which the session is told
	// as policy.ReasonProjectNotNamed and the request.created audit row records apart.
	projectMissing []string
}

// denial is one denied name of a request and the rule behind the deny (policy.Decision.Reason,
// or policy.ReasonSignInOff or policy.ReasonNoReason for a name this pass denied in place of
// putting it to a person).
// A deny the policy cannot name a reason for makes no denial at all, since there is nothing to
// tell the session beyond the refusal itself; it sets evaluation.unexplained instead.
type denial struct{ name, reason string }

// evaluate is the policy's answer for each of names to requester, read against the request's own
// reason. A name whose outcome would be an approval is denied instead when nobody could be asked:
// policy.ReasonSignInOff when the broker signs nobody in (SignInOff), and otherwise
// policy.ReasonNoReason when reason is empty once trimmed, since nobody is asked to approve work
// the record does not describe.
func (m *Machine) evaluate(set *policy.Set, names []string, requester policy.Requester, reason string) (evaluation, error) {
	e := evaluation{decisions: make([]SecretDecision, 0, len(names)), state: "granted"}
	// refusal is why a name whose outcome would be an approval is denied instead, "" when it is
	// put to its approver. Sign-in off comes first: a reason would not change it.
	var refusal string
	switch {
	case m.SignInOff:
		refusal = policy.ReasonSignInOff
	case strings.TrimSpace(reason) == "":
		refusal = policy.ReasonNoReason
	}
	denied := false
	for _, name := range names {
		d, err := set.Evaluate(name, requester)
		if err != nil {
			return evaluation{}, err
		}
		switch {
		case d.Outcome == policy.Deny:
			denied = true
			if d.Reason == policy.ReasonProjectMissing {
				e.projectMissing = append(e.projectMissing, name)
			}
			if d.Reason != "" {
				// The session is told the caller's reason (policy.CallerReason): a missing
				// project reads as an unnamed private one, so the refusal never says whether a
				// project exists; the request.created row keeps the difference.
				e.denials = append(e.denials, denial{name: name, reason: policy.CallerReason(d.Reason)})
			} else {
				e.unexplained = true
			}
		case d.Outcome == policy.Approval && refusal != "":
			denied = true
			d.Outcome = policy.Deny
			e.denials = append(e.denials, denial{name: name, reason: refusal})
		case d.Outcome == policy.Approval:
			if e.approver != "" && e.approver != d.Approver {
				return evaluation{}, ErrMixedApprovers
			}
			e.approver = d.Approver
		}
		e.decisions = append(e.decisions, SecretDecision{Name: name, Decision: d.Outcome, Source: d.Source, AirlockDigest: set.Secrets[name].AirlockDigest})
	}
	switch {
	case denied:
		e.state = "denied"
	case e.approver != "":
		e.state = "pending"
	}
	return e, nil
}

// newRequest is one request row as Create writes it. lifetime is the seconds its grant lasts, nil
// for a grant that lasts until its session ends.
type newRequest struct {
	id, enrollmentID, reason, state, approver, rulesVersion, sessionID, recordID string
	lifetime                                                                     *int
	decisions                                                                    []SecretDecision
	// denials are the denied names a reason was named for (evaluation.denials), which
	// decisionDetail turns into the row's decision_detail, as it does unexplained: whether some
	// other name was denied with no reason to name.
	denials     []denial
	unexplained bool
	// projectMissing are the names denied because their project tag names a key Dispatch holds
	// no project for: the session reads them as project-not-named, and the audit row and the
	// grant-log line name them apart for the broker's operator (auditDetail).
	projectMissing []string
	// requester is the session as the policy saw it when this very request was decided
	// (Machine.requester), so the request.created audit row names the person and the service the
	// decision was made under rather than whoever a second read would answer later.
	requester policy.Requester
	// pendingExpiresAt is set only when state == "pending"; it is also the credential-request
	// record's own ExpiresAt, computed once so the two never drift a few microseconds apart.
	pendingExpiresAt time.Time
	// notify is the person whose Inbox alone lists a pending request that waits on
	// record.AnyoneApprover (responsiblePerson), "" for everyone's; createPending writes it as
	// credential_requests.notify, which PendingForApprover reads. It is no part of the signed
	// record: it chooses who is asked, never who may decide.
	notify string
	// judgeHolds is whether the model judge holds the pending request: insertRequest then writes
	// requests.judge_until JudgeHold after the database's own now(), the clock every read of the
	// hold compares it with, and leaves it null for one a person decides.
	judgeHolds bool
}

func (r newRequest) names() []string {
	names := make([]string, len(r.decisions))
	for i, d := range r.decisions {
		names[i] = d.Name
	}
	return names
}

// decisionDetail is the row's decision_detail, which Create also answers as the request's own
// Detail: for a denied request, each denied name a reason was named for as
// "<NAME>: <reason>", several joined with "; " in the request's own name order, and the fixed
// text of every deny the policy cannot name whenever one occurred, last, so such a refusal reads
// as it always has and a session told to ask again with a reason is not left to find that its
// second ask is refused too. Every other state records none: only a denial has something to
// explain.
func (r newRequest) decisionDetail() string {
	if r.state != "denied" {
		return ""
	}
	parts := make([]string, 0, len(r.denials)+1)
	for _, d := range r.denials {
		parts = append(parts, d.name+": "+d.reason)
	}
	if r.unexplained || len(parts) == 0 {
		parts = append(parts, "policy denies at least one requested secret")
	}
	return strings.Join(parts, "; ")
}

// auditDetail is the request.created audit row's detail: the request's state, the names it asked
// for, and who asked — person for the operator whose session it is, service for the one its pod
// or machine proved (newRequest.requester, the very requester the request was decided under). A
// key whose value is empty is left out rather than written as "", since the grant-log route reads
// service off this row and a request no service made should answer nothing there rather than an
// empty service name.
func (r newRequest) auditDetail() auditDetail {
	detail := auditDetail{"state": r.state, "secrets": r.names()}
	if r.requester.Operator != "" {
		detail["person"] = r.requester.Operator
	}
	if r.requester.Service != "" {
		detail["service"] = r.requester.Service
	}
	if len(r.projectMissing) > 0 {
		detail["project_missing"] = r.projectMissing
	}
	return detail
}

// createdRequestLogEvent is the request.created audit row insertRequest wrote for this request,
// as the grant-log line Create logs once its transaction has committed. Every field it
// carries is one the decision itself was made with: the requester the policy saw, the request's
// names and their decisions in name order — the order store.ReadGrantLog answers them in, so the
// line and the route's event read the same way round — and the audit row's own detail. It is
// named for the row it carries, as endedRequestLogEvent is, so a reader at the call site tells
// it from the package-level grantLogEvent it builds on.
func (r newRequest) createdRequestLogEvent(enr enrollmentRow) store.GrantLogEvent {
	ev := grantLogEvent("request.created", "session:"+r.enrollmentID, enr)
	ev.RequestID = r.id
	ev.Service = r.requester.Service
	ev.Secrets = make([]store.GrantLogSecret, len(r.decisions))
	for i, d := range r.decisions {
		ev.Secrets[i] = store.GrantLogSecret{Name: d.Name, Decision: d.Decision}
	}
	slices.SortFunc(ev.Secrets, func(a, b store.GrantLogSecret) int { return strings.Compare(a.Name, b.Name) })
	ev.State = r.state
	ev.Approver = r.approver
	if r.lifetime != nil {
		ev.LifetimeSeconds = *r.lifetime
	}
	ev.Reason = r.reason
	ev.Detail = r.decisionDetail()
	ev.Raw = rawJSON(r.auditDetail())
	return ev
}

// insertRequest writes the request row, its per-secret decisions, and its request.created audit
// row. A pending row's record_id is filled in by createPending in the same transaction.
func (m *Machine) insertRequest(ctx context.Context, tx pgx.Tx, r newRequest) error {
	var pendingExpires *time.Time
	var holdSeconds *int
	if r.state == "pending" {
		t := r.pendingExpiresAt
		pendingExpires = &t
		if r.judgeHolds {
			s := int(JudgeHold / time.Second)
			holdSeconds = &s
		}
	}
	detail := r.decisionDetail()
	if _, err := tx.Exec(ctx, `insert into requests (id, enrollment_id, reason, state, allowed_approver, rules_version, lifetime_seconds, pending_expires_at, session_id, record_id, decided_at, decision_detail, judge_until)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, case when $4 in ('granted','denied') then now() end, $11, now() + $12::int * interval '1 second')`,
		r.id, r.enrollmentID, r.reason, r.state, nullable(r.approver), r.rulesVersion, r.lifetime, pendingExpires, nullable(r.sessionID), nullable(r.recordID), nullable(detail), holdSeconds); err != nil {
		return err
	}
	for _, d := range r.decisions {
		if _, err := tx.Exec(ctx, `insert into request_secrets (request_id, name, decision, source, airlock_digest) values ($1,$2,$3,$4,$5)`, r.id, d.Name, d.Decision, d.Source, d.AirlockDigest); err != nil {
			return err
		}
	}
	return audit(ctx, tx, "request.created", r.enrollmentID, r.id, nil, "session:"+r.enrollmentID, r.auditDetail())
}

// createPending writes, in Create's transaction, a pending request and its credential-request
// record, with the person the record is listed for (r.notify, NULL for everyone).
func (m *Machine) createPending(ctx context.Context, tx pgx.Tx, enr enrollmentRow, r newRequest, obj record.RequestObject) (Request, error) {
	body := record.Body{
		Request:         obj.Compact,
		Approver:        r.approver,
		Enrollment:      record.Enrollment{Kind: enr.Kind, RuntimeID: enr.RuntimeID, Operator: deref(enr.Operator), Slot: enr.Slot},
		LifetimeSeconds: r.lifetime,
		RulesVersion:    r.rulesVersion,
		ExpiresAt:       r.pendingExpiresAt,
	}
	recordID := body.ID()
	if _, err := tx.Exec(ctx, `insert into credential_requests (id, body, kind, approver, enrollment_id, expires_at, notify) values ($1,$2,'agent_secret',$3,$4,$5,$6)`,
		recordID, body.Canonical(), body.Approver, r.enrollmentID, body.ExpiresAt, nullable(r.notify)); err != nil {
		return Request{}, err
	}
	r.recordID = recordID
	if err := m.insertRequest(ctx, tx, r); err != nil {
		return Request{}, err
	}
	return Request{ID: r.id, State: "pending", Secrets: r.decisions, RecordID: &recordID, Approver: &body.Approver, LifetimeSeconds: r.lifetime}, nil
}

// lockPendingNames takes one advisory lock for each requested secret in this enrollment, in sorted
// order, until tx ends. It lets an exact repeat coalesce but refuses any overlapping request that
// would create a second card for the same approver and secret.
func lockPendingNames(ctx context.Context, tx pgx.Tx, enrollmentID string, sorted []string) error {
	for _, name := range sorted {
		key, err := json.Marshal([]string{enrollmentID, name})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 0))`, string(key)); err != nil {
			return fmt.Errorf("lock pending requests: %w", err)
		}
	}
	return nil
}

// Cancel ends a still-pending request the requesting session no longer wants, writing the
// transition, its audit row, and — when the request has a credential-request record — the
// record's cancelled event, all in one transaction (store.EndPendingRequests).
// The owner check compares the caller's owner with the owner of the enrollment the request hangs
// off now (Machine.owner, as OwnerAndState reads it), which an adoption rewrites: any enrollment
// of the same person, Legion claim or machine cancels it, and an enrollment that records no owner
// cancels its own requests alone, so the moved session cancels the request that moved with it and
// a box it moved off that records no owner is ErrNotOwner. A person's pod cancels its person's
// requests only while BROKER_POD_OPERATORS maps its account to that person (unmappedPersonPod).
// After the setting drops the account, or maps it to another person, the pod cancels its own
// requests alone, and its person cancels none of the pod's requests.
//
// The audit row EndPendingRequests writes names the enrollment the request hangs off, and the
// grant-log line names it too, since the line carries the ids its row names; the actor on both is
// the caller's session. The request's detail says which of the two cancelled it: its own session,
// or another session of its owner.
//
// Before the owner check, on the pool and since it may ask Dispatch, a session that does not hold
// the request is answered as for an id no request has, pgx.ErrNoRows, when the request touches a
// private project it cannot see, whoever owns it, and ErrProjectAccessUnavailable when Dispatch
// could not say (HideRequestFromSession): so ErrNotOwner says only that a request exists in a
// project the caller sees.
func (m *Machine) Cancel(ctx context.Context, id, enrollmentID string) error {
	if err := m.HideRequestFromSession(ctx, id, enrollmentID); err != nil {
		return err
	}
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state, requestEnrollment string
	owner, err := m.scanOwner(tx.QueryRow(ctx, `select `+ownerColumns("e")+`, r.state, r.enrollment_id::text from requests r join enrollments e on e.id = r.enrollment_id
		where r.id=$1 for update of r`, id), &state, &requestEnrollment)
	if err != nil {
		return err
	}
	caller, err := m.scanOwner(tx.QueryRow(ctx, `select `+ownerColumns("e")+` from enrollments e where e.id=$1`, enrollmentID))
	if err != nil {
		return err
	}
	if owner != caller {
		return ErrNotOwner
	}
	if state != "pending" {
		return ErrTerminal
	}
	actor := "session:" + enrollmentID
	detail := "cancelled by the requesting session"
	if requestEnrollment != enrollmentID {
		detail = "cancelled by another session of the owner that made it"
	}
	end := store.RequestEnd{State: "cancelled", Actor: actor, DecidedBy: &actor, Detail: detail}
	if _, err := store.EndPendingRequests(ctx, tx, store.RequestByID, id, end); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	store.LogGrantEvent(endedRequestLogEvent(end, requestEnrollment, id))
	return nil
}

// DismissedMessage is logged, at INFO, once for each call of Dismiss that dismissed a record or
// found it dismissed already, followed by record_id=<the record>, kind=<agent_secret or
// launcher_credential>, login=<the canonical login that asked> and wrote=<whether this call wrote
// the dismissal, false when an earlier one had>. A refused dismissal logs nothing.
const DismissedMessage = "broker: an expired credential request was dismissed"

// Dismissal is the one dismissal of an expired credential-request record: the record, who
// dismissed it (a person's canonical login, or broker for a record migration 0033 found already
// out of the lists), and when.
type Dismissal struct {
	RecordID    string
	DismissedBy string
	DismissedAt time.Time
	// Wrote is whether this call wrote the dismissal. It is false when an earlier dismissal had,
	// whose values the fields above then are.
	Wrote bool
}

// Dismiss takes an expired record out of every approver's list (PendingForApprover with
// PendingStates.Expired) for good, on the word of login, and answers the record's one dismissal:
// the one this call wrote, or, for a record dismissed already, the earlier one, unchanged. An
// expired record is one whose deadline passed before anybody decided it (recordStateSQL, the
// rule the list reads), of either kind: nothing is left to decide on it, and its agent asks again
// with a new request. A record is dismissed once, for every list that holds it, so a shared
// request one person dismisses leaves everybody's list.
//
// Its refusals are pgx.ErrNoRows for an id that names no record, and for a secret request login
// may not see (HideUnseen: one of a private project they are not on), which is the answer every
// other read of the record gives them, so a person outside a private project learns nothing of its
// records from a dismissal that names one; ErrProjectAccessUnavailable where Dispatch could not
// say whether login may see it; record.ErrNotApprover when the record's approver is neither
// record.CanonicalLogin(login) nor record.AnyoneApprover, which is the list's own rule for whose
// list holds a record, and for an empty login or one that is record.AnyoneApprover itself, which
// name nobody; ErrNotExpired for a record nobody has decided whose deadline has not passed; and
// ErrTerminal for every other record, one approved, denied or revoked, and a secret request
// cancelled before its deadline. A secret request cancelled after its deadline is expired, and is
// dismissed like any other.
//
// The person rule is read on the pool before the transaction (HideUnseen), since it may ask
// Dispatch, and no call to Dispatch runs in a transaction that holds locks. It reads the record's
// rows rather than its signed request, so a record signed for another broker address is refused
// an outsider as an unknown id too, not with the error of a request this broker cannot verify.
//
// It is one transaction, and it reads the record's state under a share lock taken before the read:
// on the requests row for a secret request, which waits for ApplyDecision's `for update` of that
// row, and on the credential_requests row for a machine login, which waits for machine's
// ApplyDecision's `for no key update` of it. So a decision that holds that lock first, one that
// began before the deadline and commits after it included, is read as what it decided, and the
// dismissal is refused ErrTerminal rather than written under it. The state is read by a statement
// of its own, after the lock, so it sees what the decision it waited for committed. A decision
// that takes the lock after this read waits for this transaction and then compares the deadline
// with its own now(), the moment its own transaction began: one that began after the deadline
// answers its expiry, and one that began before it may still decide the record. That record is
// then decided with a dismissal row beside it, which nothing reads: the list reads a dismissal
// only for a record recordStateSQL reads as expired, and a later Dismiss refuses a decided record
// ErrTerminal before it reads one. Two dismissals of one record meet on the table's primary key:
// the first's insert returns the row it wrote, and the second inserts nothing and reads the first
// one's row back.
func (m *Machine) Dismiss(ctx context.Context, recordID, login string) (Dismissal, error) {
	if err := m.HideUnseen(ctx, recordID, login); err != nil {
		return Dismissal{}, err
	}
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return Dismissal{}, err
	}
	defer tx.Rollback(ctx)
	var kind, approver string
	if err := tx.QueryRow(ctx, `select kind, approver from credential_requests where id=$1`, recordID).Scan(&kind, &approver); err != nil {
		return Dismissal{}, err
	}
	login = record.CanonicalLogin(login)
	if login == "" || login == record.AnyoneApprover || approver != login && approver != record.AnyoneApprover {
		return Dismissal{}, record.ErrNotApprover
	}
	lock := `select 1 from credential_requests where id=$1 for share`
	if kind == record.KindAgentSecret {
		lock = `select 1 from requests where record_id=$1 for share`
	}
	if _, err := tx.Exec(ctx, lock, recordID); err != nil {
		return Dismissal{}, err
	}
	var state *string
	if err := tx.QueryRow(ctx, `select `+recordStateSQL+` from credential_requests cr
		left join requests req on req.record_id = cr.id where cr.id=$1`, recordID).Scan(&state); err != nil {
		return Dismissal{}, err
	}
	switch {
	case state == nil:
		return Dismissal{}, ErrTerminal
	case *state != "expired":
		return Dismissal{}, ErrNotExpired
	}
	d := Dismissal{RecordID: recordID}
	err = tx.QueryRow(ctx, `insert into credential_request_dismissals (record_id, dismissed_by) values ($1, $2)
		on conflict (record_id) do nothing returning dismissed_by, dismissed_at`, recordID, login).
		Scan(&d.DismissedBy, &d.DismissedAt)
	d.Wrote = err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `select dismissed_by, dismissed_at from credential_request_dismissals where record_id=$1`, recordID).
			Scan(&d.DismissedBy, &d.DismissedAt)
	}
	if err != nil {
		return Dismissal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Dismissal{}, err
	}
	slog.Info(DismissedMessage, "record_id", recordID, "kind", kind, "login", login, "wrote", d.Wrote)
	return d, nil
}

// ExpirePending expires every pending request past its deadline, writing the state transition,
// its audit row, and its record's expired event together.
func (m *Machine) ExpirePending(ctx context.Context, now time.Time) (int, error) {
	expired, err := m.expirePending(ctx, now)
	return len(expired), err
}

// expirePending is ExpirePending's shared implementation: it returns the rows it moved to
// 'expired' so Sweeper (this package's own Tick) can wake each one's owner once the transaction
// has actually committed.
func (m *Machine) expirePending(ctx context.Context, now time.Time) ([]store.EndedRequest, error) {
	return m.endPending(ctx, store.RequestsPastDeadline, now, store.RequestEnd{
		State: "expired", Actor: "broker", DecidedAt: &now, Detail: "no answer before the request expired",
	})
}

// SignInOffActor is what a request the sweep denied on a broker with BROKER_SIGNIN=off records as
// having decided it: its decided_by, its record's denied event's actor and its request.denied
// audit row's actor, since no person decided it and nobody could have.
const SignInOffActor = "broker:signin-off"

// denyUndecidable is the sweep's half of SignInOff: Create writes no request pending on a broker
// that signs nobody in, but the database can hold ones a broker with sign-in on wrote, before a
// restart with sign-in off or from a pod with sign-in on that runs beside this one in a roll
// between the two settings. Each one this broker would list for its approver
// (PendingForApprover: its signed request verifies for this broker's own audience) is denied
// here, policy.ReasonSignInOff, with its record's denied event and its request.denied audit row
// in one statement, rather than left on the approver's list behind a page that is gone until its
// deadline. The sweep runs expirePending first, so an overdue request is normally expired, not
// denied; if that expiry fails, this still denies it. No time bounds the selection: while this
// pod runs it denies what a pod with sign-in on queues too, so a roll between the two settings
// denies each request of its overlap that such a pod, by a person or its model judge, did not
// approve first, each one's session told why, and a session asks
// again once the roll is done. A bound taken per process cannot do better, since a later or
// restarted pod with sign-in off takes a later one.
// A request signed for another BROKER_PUBLIC_URL is left alone: it belongs to another broker
// sharing this database, whose page may still be served, and this broker lists it for nobody
// anyway. Create ends such a request on its session's next ask of this broker for one of its
// names that writes a request or that a live grant answers (BrokerMovedDetail). With sign-in off
// no request waits, so such an ask is decided at once, denied where a name would have waited for
// a person, and it ends that request too. A request that no valid ask reaches stays pending until
// it expires. It returns the requests it denied, so the sweep wakes each one's session once the
// transaction has committed, and nothing on a broker with sign-in on. A grant made before stays
// as it was: off decides nothing new, it takes nothing back.
func (m *Machine) denyUndecidable(ctx context.Context, now time.Time) ([]store.EndedRequest, error) {
	if !m.SignInOff {
		return nil, nil
	}
	rows, err := m.Store.Pool.Query(ctx, pendingRecordsQuery)
	if err != nil {
		return nil, err
	}
	var undecidable []string
	var id, canonical string
	var createdAt time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &canonical, &createdAt}, func() error {
		if _, _, err := m.requestedIdentifiers(canonical, createdAt); err == nil {
			undecidable = append(undecidable, id)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if len(undecidable) == 0 {
		return nil, nil
	}
	actor := SignInOffActor
	return m.endPending(ctx, store.RequestsByIDs, undecidable, store.RequestEnd{
		State: "denied", Actor: actor, DecidedBy: &actor, DecidedAt: &now, Detail: policy.ReasonSignInOff,
	})
}

// pendingRecordsQuery is every pending agent_secret request with its record's canonical body and
// the time the record was written, which is what requestedIdentifiers verifies a record against.
const pendingRecordsQuery = `select r.id::text, cr.body, cr.created_at from requests r
	join credential_requests cr on cr.id = r.record_id where r.state = 'pending'`

// endPending ends, as end says, every request scope and arg select that is still pending, in a
// transaction of its own, and logs one grant-log line per request once that transaction has
// committed, since one audit row was written per request. It answers the requests it ended, so
// the sweep wakes each one's owner after the commit too. expirePending and denyUndecidable are
// its callers.
func (m *Machine) endPending(ctx context.Context, scope store.PendingScope, arg any, end store.RequestEnd) ([]store.EndedRequest, error) {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	ended, err := store.EndPendingRequests(ctx, tx, scope, arg, end)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	for _, r := range ended {
		store.LogGrantEvent(endedRequestLogEvent(end, r.EnrollmentID, r.ID))
	}
	return ended, nil
}

// ApplyDecision decides a pending agent_secret record by login, the login of the person deciding
// it on the broker's own approval page (approval.Page.decide, its one caller). approve=true mints
// the grant while the requesting enrollment may still take one — live, dormant and holding what
// the session's next box will adopt, or lapsed with no end written against it yet, where the end
// the sweep is about to write would leave it dormant (mayGrant below) — and otherwise the
// request is denied.
// It re-reads the record body, recomputes its id, refuses any login but the record's own approver
// (record.ErrNotApprover) whatever the record's state, and an approval by any login the current
// policy would not have approve one of the request's names (currentPolicyAdmits). A denial
// releases nothing, so the record's approver may still deny a request an owner change left them
// unable to approve. An approval of a name the current policy denies because the secret's
// requesters tag no longer lists the session is neither refused nor granted: nobody could
// approve it back, so the request is recorded denied by the broker itself (PolicyDeniedActor)
// with that reason as its detail, and the event still names the person who pressed approve.
// It re-verifies the embedded request object, and writes the event (naming
// that login), the request transition and the audit row in one transaction. For its approver, a
// record that expired undecided — expired by the sweeper, or past its pending_expires_at before
// the sweeper got to it — is ErrExpired, and any other non-pending record ErrTerminal: a
// duplicate or late decision changes nothing.
//
// The enrollment the decision grants to is the one the `requests` row names, never the one
// `credential_requests` recorded. The record is append-only (credential_requests_refuse_update,
// migration 0005), so nothing can re-point it when a session moves to another box, while the
// request row moves with the grant: enroll.Create hands what a predecessor held to the next
// enrollment of that session name under the same principal. Reading the record's own column
// would answer an approval given after a move with "the requesting enrollment is no longer live"
// and mint nothing, which is the half of the incident dispatch://INFRA-1167 reports.
//
// The work is one attempt (applyDecisionAttempt), made again when the request moves out from
// under it. A session moves once per enrolment, so the attempt after a move reads one enrollment
// under both of its locks; the bound on how many are made is the one enroll.Service.Create puts
// on its own retries.
func (m *Machine) ApplyDecision(ctx context.Context, recordID string, approve bool, login string) (Decision, error) {
	return m.applyDecision(ctx, recordID, approve, byPerson(login))
}

// ApplyJudgeDecision approves recordID as the model judge running as modelID, writing everything
// a person's approval writes except that no person is named in any of it: the decision event
// carries a null login and the judge's actor, the request row records that actor as having
// decided it, and the grant records the record's own approver — the owner's login, or
// record.AnyoneApprover — since the grant is the one that approver would have made and the judge
// keeps nothing of it afterwards (D1).
//
// judged is the judge's verdict, written as one request.judged audit row inside the approval's
// own transaction, so a grant and the record of why it exists commit together or not at all
// (D11). It is a map[string]any rather than this package's own auditDetail because the judge
// that calls this lives outside the package and must be able to name the type; the two types
// are one. The row written from it carries one key the judge does not write: applied, set from
// what this transaction did with the approval rather than from what the judge asked for, since
// an approval can commit and grant nothing (decider.judgedRow). The caller's own map is left
// as it was.
//
// This is the judge's one approval. A record the airlock rules do not let it decide is refused
// with record.ErrNotJudgeable and nothing is written; a record a person decided first, or one
// that expired, comes back as ErrTerminal or ErrExpired, and one the judge no longer holds as
// ErrJudgeHoldEnded, which are the ordinary answers for a judge that lost the race and no fault
// of its (D10). Each of those writes nothing, and the judge records its reasoning with LogJudged,
// which hands a request it holds to its person, or with DenyJudged, which refuses it.
func (m *Machine) ApplyJudgeDecision(ctx context.Context, recordID, modelID string, judged map[string]any) (Decision, error) {
	return m.applyDecision(ctx, recordID, true, byJudge(modelID, auditDetail(judged)))
}

// JudgeHold is how long the model judge holds a request it decides, from the moment the request
// is written (requests.judge_until): while it lasts the request is on no person's list and no
// person may decide it, and after it the judge may write nothing more. A run is bounded at
// runBudget, 35 seconds (judge.runBudget), so a hold runs out in two cases: the broker lost the
// run (it stopped serving mid-run), or the run's approval answered nothing and LogJudged could not
// read whether it applied, a row that ends no hold (JudgedAppliedUnknown). The request is then its
// person's, on their list.
const JudgeHold = 2 * time.Minute

// ErrJudgeHolds is a person's decision refused while the model judge holds the request.
var ErrJudgeHolds = errors.New("the model judge is deciding this request; it goes to its person if the judge cannot decide it")

// ErrJudgeHoldEnded is the judge's own write refused on a request it does not hold.
var ErrJudgeHoldEnded = errors.New("the model judge no longer holds this request")

// DenyJudged ends recordID as the model judge running as modelID refusing it: the one way a
// request the judge holds ends other than in the judge's grant, its hand-over to its person
// (LogJudged) or the end of the hold. why is the sentence its agent reads in the request's
// decision detail — what the judge found, and that this request will not be granted — since no
// person is asked about it. judged is the judge's verdict, written as the request.judged row in
// the same transaction, as ApplyJudgeDecision writes it beside an approval.
func (m *Machine) DenyJudged(ctx context.Context, recordID, modelID string, judged map[string]any, why string) (Decision, error) {
	who := byJudge(modelID, auditDetail(judged))
	who.refusal = why
	return m.applyDecision(ctx, recordID, false, who)
}

// judgeKey reports whether the model judge decides a request for name: the airlock rules give
// the judge seconds of it, and it carries no administrator's tag, of which a person approves every
// release whatever the rules say.
func (m *Machine) judgeKey(set *policy.Set, name string) (uint32, bool) {
	seconds, judged := m.Airlock.JudgeSeconds(name)
	if !judged || set.Secrets[name].Admin {
		return 0, false
	}
	return seconds, true
}

// namesAJudgeKey reports whether any of names is a judge key (judgeKey): the requests that may be
// the judge's at all, and the only ones Create evaluates before its transaction.
func (m *Machine) namesAJudgeKey(set *policy.Set, names []string) bool {
	return slices.ContainsFunc(names, func(name string) bool {
		_, judged := m.judgeKey(set, name)
		return judged
	})
}

// judgeDecides reports whether the model judge decides a pending request: every key in it that
// needs a decision is a judge key. A request with any key a person decides is that person's whole.
func (m *Machine) judgeDecides(set *policy.Set, decisions []SecretDecision) bool {
	needs := 0
	for _, d := range decisions {
		if d.Decision != policy.Approval {
			continue
		}
		needs++
		if _, judged := m.judgeKey(set, d.Name); !judged {
			return false
		}
	}
	return needs > 0
}

// judgeLifetime bounds a request's lifetime by the judge seconds of each of names, the keys the
// judge decides, as a max-grant tag bounds it: a request that named no lifetime gets the shortest
// of them, when it is shorter than lifetime, the default it would otherwise get, and one that
// asked for longer is refused, naming the --ttl that would do. Create calls it with the keys that
// need a decision alone (decidedNames), before the max-grant tags are read.
func (m *Machine) judgeLifetime(set *policy.Set, names []string, signed, lifetime *int) (*int, error) {
	bound, bounding := 0, ""
	for _, name := range names {
		if seconds, judged := m.judgeKey(set, name); judged && (bounding == "" || int(seconds) < bound) {
			bound, bounding = int(seconds), name
		}
	}
	if bounding == "" {
		return lifetime, nil
	}
	limit := formatGrantDuration(time.Duration(bound) * time.Second)
	if signed != nil && *signed > bound {
		return nil, fmt.Errorf("%w: %s's judge grants at most %s; ask with agent-secrets --ttl %s", ErrGrantRequiresLifetime, bounding, limit, limit)
	}
	if lifetime == nil || *lifetime > bound {
		return &bound, nil
	}
	return lifetime, nil
}

// applyDecision is the retry loop both entry points share: one attempt, made again when the
// request moves to another enrollment under it (applyDecisionAttempt's moved).
func (m *Machine) applyDecision(ctx context.Context, recordID string, approve bool, who decider) (Decision, error) {
	const maxAttempts = 3
	for range maxAttempts {
		decision, moved, err := m.applyDecisionAttempt(ctx, recordID, approve, who)
		if moved {
			continue
		}
		return decision, err
	}
	return Decision{}, fmt.Errorf("decide record %s: exhausted retries while the request kept moving to another enrollment", recordID)
}

// judgedKind is the audit kind of the model judge's own verdict on a record: what it was asked,
// what it answered and why, as the judge wrote it. It is free text in the audit table, as every
// request.* kind is, and is spelled here alone.
const judgedKind = "request.judged"

// LogJudged writes the model judge's verdict on recordID alone — one request.judged audit row, in
// a transaction of its own, against the request the record names and the enrollment that request
// hangs off now — and hands a request the judge held to its person: it ends the hold
// (judge_until) in the same transaction, unless the row says applied unknown (below), which ends
// no hold. It writes no decision event, no grant and no state change.
//
// There are two ways to write that row because a verdict and a decision are two things.
// ApplyJudgeDecision writes the verdict inside the approval it makes, so the reasoning cannot be
// lost from the grant it produced (D11), and DenyJudged inside the refusal it makes. This one is
// for every verdict that decides nothing: a run that could not decide — the model did not answer,
// its answer did not parse, a read it needed failed, the rules, the policy or a project moved
// under it — and an approval the state machine then refused, where that transaction rolled back
// and wrote nothing at all. The reasoning is worth keeping either way: it is the record of what
// the judge was asked and what it answered.
//
// A row whose applied is AppliedUnknown reports an approval whose COMMIT failed (ErrCommitUncertain):
// it may have applied on the server and only its answer been lost, and then its own row is already
// there, saying what it did, or is still committing. So LogJudged resolves it, inside its own
// transaction (approvalLanded), after taking the request's row for share, which waits for a COMMIT
// or an abort in flight, since the approval holds that row for update until it ends: when the
// approval's own row is there, it writes nothing and ends no hold (JudgedAlreadyApplied), so the
// trail holds the one row that says what happened; when it is not, the approval did not apply, and
// the row is written with applied false (JudgedWritten); when that wait outlasts its bound or the
// read fails, it writes the row with applied AppliedUnknown and the cause under applied_unknown
// (JudgedAppliedUnknown), in a transaction of its own, since a read that lost its connection
// leaves the first one nothing to write on, and ends no hold: the approval it could not read may
// still hold the request's row, and its hold runs out by itself (JudgeHold), which hands the
// request to its person. Every other row is written as given (JudgedWritten), with no read: an
// approval the state machine refused before its COMMIT certainly wrote nothing.
func (m *Machine) LogJudged(ctx context.Context, recordID, modelID string, judged map[string]any) (JudgedWrite, error) {
	detail := judgedDetail(judged)
	wrote := JudgedWritten
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if applied, ok := detail["applied"].(string); ok && applied == AppliedUnknown {
		landed, err := m.approvalLanded(ctx, tx, recordID)
		switch {
		case err != nil:
			detail["applied_unknown"] = record.Sanitize(err.Error(), record.MaxReasonRunes)
			wrote = JudgedAppliedUnknown
			// A read the database ended (lock_timeout) leaves tx as it was, but one whose
			// connection was lost (the network, a backend a failover ended) leaves tx with
			// no connection to write on, so the row is written in a transaction of its own
			// either way: the row is the one write a run that asked the model must keep.
			_ = tx.Rollback(ctx)
			if tx, err = m.Store.Pool.Begin(ctx); err != nil {
				return "", err
			}
		case landed:
			return JudgedAlreadyApplied, nil
		default:
			detail["applied"] = false
		}
	}
	// The enrollment is the one the request hangs off now, never the one the record froze, as
	// every other read of this package is (the package comment's moved session). No lock and no
	// liveness test: the row is written for the trail, and a verdict on a request whose box has
	// gone is as worth keeping as one on a live request.
	var requestID string
	var enr enrollmentRow
	if err := tx.QueryRow(ctx, `select req.id, e.id, e.kind, e.operator, c.service, e.runtime_id, coalesce(e.session_id,'')
		from requests req join enrollments e on e.id = req.enrollment_id
		left join launcher_credentials c on c.id = e.launcher_credential_id where req.record_id=$1`, recordID).
		Scan(&requestID, &enr.ID, &enr.Kind, &enr.Operator, &enr.Service, &enr.RuntimeID, &enr.SessionID); err != nil {
		return "", err
	}
	actor := record.JudgeActor(modelID)
	if err := audit(ctx, tx, judgedKind, enr.ID, requestID, nil, actor, detail); err != nil {
		return "", err
	}
	// The hand-over: a verdict that decides nothing ends the judge's hold in the transaction
	// that records it, so the request is on its person's list the moment the row is, and
	// theirs to decide. A request no judge held is left as it was. A row whose applied is unknown
	// ends no hold: the approval it could not read may still hold the request's row for update,
	// and waiting on it would spend the time the row needs; the hold runs out by itself.
	if wrote != JudgedAppliedUnknown {
		if _, err := tx.Exec(ctx, `update requests set judge_until = null where id=$1 and judge_until is not null`, requestID); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	logGrantRow(judgedKind, actor, enr, requestID, "", rawJSON(detail))
	return wrote, nil
}

// approvalLanded reports whether the request recordID names carries the request.judged row an
// approval's own transaction writes (decider.judgedRow): one whose applied is a boolean that
// transaction set, and which says nothing of why it was not applied. Every row LogJudged writes
// for an approval says why (not_applied), and a referral's says nothing of applied at all, so only
// an approval that committed leaves such a row, and a request is approved at most once.
//
// It reads inside tx, in a savepoint of its own: first the request's row for share, which waits
// for an approval whose transaction holds that row for update (applyDecisionAttempt) to commit or
// abort, so the read after it sees the state that settled; then the read itself. The wait is
// bounded by lock_timeout at half of what is left of ctx (landedWait), so the other half is left
// to write the row; a wait it ends is an error, and the savepoint's rollback leaves tx to write.
func (m *Machine) approvalLanded(ctx context.Context, tx pgx.Tx, recordID string) (bool, error) {
	if m.testLandedRead != nil {
		if err := m.testLandedRead(tx.Conn().PgConn().PID()); err != nil {
			return false, err
		}
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer sp.Rollback(ctx)
	if _, err := sp.Exec(ctx, `select set_config('lock_timeout', $1, true)`, fmt.Sprintf("%dms", landedWait(ctx).Milliseconds())); err != nil {
		return false, err
	}
	var one int
	if err := sp.QueryRow(ctx, `select 1 from requests where record_id=$1 for share`, recordID).Scan(&one); err != nil {
		return false, fmt.Errorf("wait for the approval's own transaction to end: %w", err)
	}
	var landed bool
	if err := sp.QueryRow(ctx, `select exists(select 1 from audit a join requests r on r.id = a.request_id
		where r.record_id=$1 and a.kind=$2 and jsonb_typeof(a.detail->'applied')='boolean' and not (a.detail ? 'not_applied'))`,
		recordID, judgedKind).Scan(&landed); err != nil {
		return false, err
	}
	return landed, nil
}

// landedWait is how long approvalLanded waits for an approval's transaction to end: half of what
// is left of ctx, or JudgeRowReserve where ctx has no deadline, and never less than a
// millisecond, since a lock_timeout of zero waits for ever.
func landedWait(ctx context.Context) time.Duration {
	wait := JudgeRowReserve
	if end, ok := ctx.Deadline(); ok {
		wait = time.Until(end) / 2
	}
	return max(wait, time.Millisecond)
}

// JudgedWrite is what LogJudged did with the row it was given.
type JudgedWrite string

const (
	// JudgedWritten is a row written as it was given.
	JudgedWritten JudgedWrite = "written"
	// JudgedAlreadyApplied is nothing written: the row said an approval was not applied, and
	// that approval's own request.judged row is there, so its COMMIT applied and only the
	// answer to it was lost.
	JudgedAlreadyApplied JudgedWrite = "already-applied"
	// JudgedAppliedUnknown is a row written with applied AppliedUnknown, since whether the
	// approval it reports applied could not be read.
	JudgedAppliedUnknown JudgedWrite = "applied-unknown"
)

// AppliedUnknown is the applied of a request.judged row whose approval may or may not have
// applied: the approval's answer was lost, and so was the read of whether its own row is there.
const AppliedUnknown = "unknown"

// judgedDetail is a caller's verdict as the audit row holds it: a copy of what the caller gave,
// and never nil. Never nil because audit.detail is NOT NULL and a nil map is a null parameter,
// so a judge that recorded no reasoning records the empty object the column defaults to and
// the approval it just made is not lost to a constraint. A copy because the row a transaction
// writes is the transaction's to finish — decider.judgedRow sets applied on it from what that
// transaction did — and the map a caller passed is still the caller's, held across the retries
// applyDecision may make. The one spare slot is that applied.
func judgedDetail(judged map[string]any) auditDetail {
	row := make(auditDetail, len(judged)+1)
	maps.Copy(row, judged)
	return row
}

// decider is who is deciding a record in one applyDecisionAttempt: a person, named by the login
// their signed-in session proved, or the model judge, named by the id of the model that decided
// and carrying the verdict it wrote. Exactly one of the two is ever set, and the constructors
// below are the only way to build one, so "a judge with a login" and "nobody at all" are not
// states a caller reaches by filling in a field.
type decider struct {
	login   string
	modelID string
	// judged is the judge's verdict, written as the request.judged audit row inside the
	// decision's own transaction (D11); nil for a person, who writes no such row.
	judged auditDetail
	// refusal is why the judge refused a request it holds (DenyJudged), written as the
	// denial's detail for its agent to read; empty for every other decision.
	refusal string
}

// byPerson is the decider of a decision a person made on the broker's own approval page: the
// login their Dispatch session names, which the record's approver rule then canonicalizes
// (record.Body.ApproverLogin).
func byPerson(login string) decider { return decider{login: login} }

// byJudge is the decider of a decision the model judge made: the id of the model and the verdict
// it wrote. No login is ever recorded for it — the judge is no person, and the person who would
// otherwise have decided is named nowhere in what it writes.
func byJudge(modelID string, judged auditDetail) decider {
	return decider{modelID: modelID, judged: judged}
}

// judgedRow is the judge's verdict as the request.judged row of this decision's own transaction
// records it: what the judge wrote, with applied set from next, the state that transaction is
// leaving the request in.
//
// The judge cannot write that key itself, and the row has to carry it: an approval the judge
// makes can commit and grant nothing. The current policy may have dropped the requester from
// the secret's list, which is recorded as the request's own denial under PolicyDeniedActor, and
// the requesting enrollment may have gone, which is recorded approved and then denied. Both
// commit, both leave the row beside them, and in both the judge granted nothing — so applied is
// read off what the transaction did and never off what the judge asked for. The row it builds
// is a copy (judgedDetail), so neither the judge's own map nor an earlier attempt's answer is
// what the next attempt writes.
func (d decider) judgedRow(next string) auditDetail {
	row := judgedDetail(d.judged)
	row["applied"] = next == "granted"
	return row
}

// isJudge reports whether this decider is the model judge rather than a person.
func (d decider) isJudge() bool { return d.modelID != "" }

// actor is the audit actor of what this decider writes: "human:<login>" or "judge:<model id>".
func (d decider) actor() string {
	if d.isJudge() {
		return record.JudgeActor(d.modelID)
	}
	return record.HumanActor(d.login)
}

// decidedBy is what the requests row records as having decided it: a person's own login, and for
// the judge its actor, since that column has no model id of its own and the judge is no login
// (D1).
func (d decider) decidedBy() string {
	if d.isJudge() {
		return record.JudgeActor(d.modelID)
	}
	return d.login
}

// eventLogin is the credential_request_events.login the decision event carries: the person's
// canonical login, and null for the judge, whose event names the model in its actor alone. The
// chain re-check reads which arm admits an approval off that actor and never off the login
// (record.ChainVerifier.Verify), and a null login is what keeps the judge's approval from ever
// being read back as some person's.
func (d decider) eventLogin() *string {
	if d.isJudge() {
		return nil
	}
	return &d.login
}

// grantApprover is the approver the grant this decision mints records: the deciding person, and
// for the judge the record's own approver — the owner's login, or record.AnyoneApprover for a
// shared secret — since the judge approves in that approver's stead and holds none of their
// rights over the grant afterwards (D1: the grant's approver is what GrantsForApprover lists and
// what mayRevoke reads).
func (d decider) grantApprover(recordApprover string) string {
	if d.isJudge() {
		return recordApprover
	}
	return d.login
}

// maySettle reports whether this decider may settle a name the current policy routes to
// policyApprover, on a record whose own approver is recordApprover (currentPolicyAdmits).
//
// A person is held to the rule they are held to everywhere: the policy's own approver for the
// name must admit them (record.MayDecide). The judge is no login and MayDecide admits none of
// them, by design; it decides in the record's approver's stead, so what is asked of it is that
// the current policy still routes the name to that same approver. Equal approvers and no others
// is the conservative reading of "pass the record's own approver" — and it has to be, since
// record.AnyoneApprover is no login either and MayDecide refuses it outright, so a shared
// secret's record could be judged no other way. Where the two disagree — an owner change, a
// secret that went shared or stopped being shared — the judge settles nothing and the request
// waits for the person who decides it today. The judge is never admitted where the record's own
// approver would not be.
func (d decider) maySettle(policyApprover, recordApprover string) bool {
	if d.isJudge() {
		return record.CanonicalLogin(policyApprover) == record.CanonicalLogin(recordApprover)
	}
	return record.MayDecide(record.KindAgentSecret, policyApprover, "", d.login)
}

// enrollmentHoldsForItsSession is "enrollment e still holds something for its session": a live
// grant, or a request nobody has answered yet. It is enroll's own holdsForItsSession word for
// word, and must stay word for word that. Three statements read the one rule — endEnrollment's
// dormancy probe, which decides whether an end of the box leaves the row dormant instead of
// revoking what it holds, adopt's predecessor query, and mayGrant's third case below — and
// mayGrant's job is to grant on a lapsed row exactly when the end the sweep is about to write
// would leave that row dormant. Were this copy the wider of the two, an approval would be
// granted onto a row the sweep then revokes; were it the narrower, an approval would be denied
// on a row the sweep then carries. Either way the answer a person gets would turn on which of
// the two ran first.
//
// It is a copy rather than one shared constant because enroll's is unexported, and a SQL
// fragment that means anything only when it is spliced against an alias named e is a poor thing
// for a package to export. What holds the copies together instead is
// TestTheLapsedRowGrantRuleAnswersWhatTheEndThatFollowsDoes, which runs this rule and
// enroll.Service.EndLapsed over every shape of row and compares their two answers, the way
// enroll's own serving_test.go pins its copy of a SQL rule against the rule itself.
const enrollmentHoldsForItsSession = `exists (select 1 from grants g where g.enrollment_id=e.id and g.revoked_at is null and (g.expires_at is null or g.expires_at > now()))
				or exists (select 1 from requests r where r.enrollment_id=e.id and r.state='pending')`

// enrollmentWouldGoDormant is "an end of this box, written now, would leave enrollment e dormant
// rather than revoke what it holds": the row names a session, records the principal that session
// belongs to, and still holds a live grant or a request nobody has answered. It is
// enroll.endEnrollment's own dormancy probe — the same two tests, in the same order, around the
// same holdsForItsSession text — because it has to answer what that probe will answer about the
// same row a moment later. Where that end finds a live pod of the row's claim it hands what the
// row holds to that pod instead of leaving it dormant (enroll.handOver), which is a carry all the
// same: a grant minted here moves with the rest.
const enrollmentWouldGoDormant = `coalesce(e.session_id,'') <> '' and e.carry_principal is not null and (
				` + enrollmentHoldsForItsSession + `)`

// applyDecisionAttempt makes one of applyDecision's attempts for who — the person or the model
// judge deciding this record — writing everything that decision writes or nothing at all. moved
// is true when the request moved to another enrollment between the read that locks the
// enrollment and the read that locks the request — an adoption committing in that window — in
// which case the attempt has written nothing and applyDecision makes another, which reads the
// enrollment the request names now.
//
// The enrollment row is locked before the request row — the same order every other
// enrollment-then-request writer in this package takes them in, so none of them deadlock — and
// the join below is what keeps that order while reading the enrollment's id off the request: it
// locks the enrollment alone (for share of e) and takes no lock on the request. Taking no lock on
// the request is what lets the request move between the two reads. An adoption holds the
// predecessor's enrollment row for update while it moves the requests off it (enroll.adopt), and
// when it commits Postgres rechecks the row this statement locked, never the request row the join
// read beside it, so the join answers with the predecessor — live, or ended and dormant — though
// the request hangs off it no longer. So the second read, under a snapshot of its own, reads the
// request's own enrollment_id, and the attempt is abandoned when the two disagree. Nothing is
// written against a box the session has moved off, and nothing is denied for a move that was
// already done when the decision arrived.
func (m *Machine) applyDecisionAttempt(ctx context.Context, recordID string, approve bool, who decider) (_ Decision, moved bool, _ error) {
	// One policy snapshot answers this decision: the default lifetime, the re-evaluation below,
	// and each name's airlock fingerprint, which the request records as what it was decided
	// under (decidedUnder). One snapshot rather than a read apiece, so a reload landing in the
	// middle of the decision cannot leave the row recording a policy no part of that decision
	// was made against. It is read before the transaction because what Dispatch holds of its
	// tagged names is resolved against it there.
	set := m.Policy.Get()
	// What Dispatch holds, read on the pool before the transaction takes its locks, and whether
	// the one deciding may decide this record at all (decisionFacts: the person rule for a person;
	// for the model judge's grant its project rule, asked again afresh and under JudgeDispatch).
	// The transaction checks that the request still hangs off the enrollment read here, and makes
	// another attempt when it moved. The judge's refusal reads none of it: a refusal grants
	// nothing, its reason reaches only the agent that asked, and the transaction reads the facts
	// only to grant (requester), so the judge refuses for good whatever project the request
	// belongs to, and a Dispatch outage cannot turn a final refusal into a hand-over.
	pre, err := m.readRowFacts(ctx, decisionKey, recordID)
	if err != nil {
		return Decision{}, false, err
	}
	// An approval asks Dispatch about the request's person too, on the pool and before the
	// transaction, as Create and every release do (restrictedOperator), for the enrollment the
	// read above found holding the request: a restricted member's request left pending from
	// before the broker asked is then the policy's denial, restricted-member, and no grant is
	// written for a release to refuse. A denial asks nothing, since it releases nothing. The
	// transaction decides by this answer only while the request still hangs off that enrollment,
	// and makes another attempt when it moved. It is asked first, on the same reads as
	// decisionFacts (decisionReads: one JudgeDispatch bound for the judge, afresh), so the
	// project facts' atDecision read is the last read of Dispatch before the transaction and the
	// facts the decision rests on are under projectaccess.CacheTTL old when it is made.
	reads, cancel := decisionReads(ctx, who)
	defer cancel()
	var restricted bool
	if approve {
		if restricted, err = m.restrictedOperator(reads, pre.holder); err != nil {
			return Decision{}, false, err
		}
	}
	var facts projectFacts
	if approve || !who.isJudge() {
		if facts, err = m.decisionFacts(reads, recordID, who, pre, set); err != nil {
			return Decision{}, false, err
		}
	}
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return Decision{}, false, err
	}
	defer tx.Rollback(ctx)

	var body string
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `select body, created_at from credential_requests where id=$1`, recordID).
		Scan(&body, &createdAt); err != nil {
		return Decision{}, false, err
	}
	// mayGrant is which of the enrollment's states a grant may be hung off, and there are three.
	// Live — not revoked and its lease not lapsed — is the ordinary one. Dormant — revoked_at set
	// and dormant_since not null: ended by an end of its box while it still held a grant or this
	// pending request, and kept for the session's next box (enroll.endEnrollment) — is the one
	// this carry is for, and a grant may land on it: an approval given while the box is gone is
	// the order dispatch://INFRA-1167 reports, the next enrollment of that session name adopts
	// the grant with the request (enroll.adopt), the carry sweep revokes it if nobody comes back
	// (enroll.Service.EndCarried), and a person's revoke of the machine login or the sign-in
	// reaches it as it reaches any grant (endCredential). The third is a row whose lease has
	// lapsed and against which no end is written yet, the window between the lapse and the sweep
	// that ends it, which is up to BROKER_SWEEP_SECONDS long: a grant lands on it when the end
	// the sweep is about to write would leave the row dormant (enrollmentWouldGoDormant, which
	// is enroll.endEnrollment's own probe). Denying there while granting a second later, on the
	// same row holding the same rows, would make the sweep's timing the person's answer.
	//
	// Ended for good is what is left: a row revoked with nothing to carry, and a lapsed row no
	// end of its box could leave dormant — one that names no session, or names one and records
	// no principal to enrol under it again, so nothing could ever adopt what it holds. An
	// approval of a request on such a row is recorded and then denied below, as it always was.
	var mayGrant bool
	var enr enrollmentRow
	if err := tx.QueryRow(ctx, `select e.id,
			e.dormant_since is not null or (e.revoked_at is null
				and (enrollment_lease_live(e.lease_expires_at) or (`+enrollmentWouldGoDormant+`))),
			e.kind, e.operator, e.subject, c.service, c.host, e.runtime_id, coalesce(e.session_id,'')
		from requests req join enrollments e on e.id = req.enrollment_id
		left join launcher_credentials c on c.id = e.launcher_credential_id where req.record_id=$1 for share of e`, recordID).
		Scan(&enr.ID, &mayGrant, &enr.Kind, &enr.Operator, &enr.Subject, &enr.Service, &enr.Host, &enr.RuntimeID, &enr.SessionID); err != nil {
		return Decision{}, false, err
	}
	if m.testAdoptionHook != nil {
		m.testAdoptionHook()
	}
	var requestID, holder, state string
	var expired, held bool
	if err := tx.QueryRow(ctx, `select id, enrollment_id, state, pending_expires_at <= now(), coalesce(judge_until > now(), false) from requests where record_id=$1 for update`, recordID).
		Scan(&requestID, &holder, &state, &expired, &held); err != nil {
		return Decision{}, false, err
	}
	if holder != enr.ID || enr.ID != pre.holder.ID {
		return Decision{}, true, nil
	}
	// The answer asked of Dispatch before the transaction is this enrollment's: the check above
	// makes another attempt when the request moved since.
	enr.Restricted = restricted
	parsed, err := verifyRecordBody(recordID, body)
	if err != nil {
		return Decision{}, false, err
	}
	// obj is the record's embedded request object, verified once per attempt through
	// verifyObject, by whichever of the two readers below comes first.
	var obj record.RequestObject
	objVerified := false
	verifyObject := func() error {
		if objVerified {
			return nil
		}
		// now is reset to the record's own creation time: the request object's own iat/exp bound
		// only how fresh it had to be when the broker first accepted it (up to 10 minutes), never
		// how long the resulting decision window may stay open (PendingTTL, hours) — re-verifying
		// it with the real current time would fail every decision made more than a few minutes
		// after creation.
		verified, err := record.VerifyRequestObject(parsed.Request, m.Audience, m.Skew, createdAt)
		if err != nil {
			return fmt.Errorf("%w: request object no longer verifies: %s", ErrGrantChainInvalid, err)
		}
		obj, objVerified = verified, true
		return nil
	}
	if who.isJudge() {
		// The judge's own check sits exactly where a person's approver check sits: before the
		// policy re-evaluation and before the state read, so a record the rules do not let the
		// judge decide is refused on the same reading that refuses a person who is not the
		// approver. It needs the verified request object, though — its one authorization
		// detail, the lifetime and the reason are what the rules bound — and a person's decision
		// does not verify that object until further down. So the judge, and the judge alone,
		// verifies it here; the verification below is the same one and runs once (verifyObject).
		// Every other check keeps the order it had, and a person's decision reads them all in
		// the order it always did.
		if err := verifyObject(); err != nil {
			return Decision{}, false, err
		}
		// The judge's grant is held to the rules' bound on what a judge decides; its refusal
		// is not, since a refusal grants nothing. Whether it may write at all is the hold's,
		// read below once the request's own state has been.
		if approve {
			// The keys the judge decided are the ones its request needed a decision for, as
			// the request's own rows hold them; a key the session got at once beside them is
			// no key of the judge's, and the release asks the live policy of it again.
			requested, err := requestedSecrets(ctx, tx, requestID)
			if err != nil {
				return Decision{}, false, err
			}
			if err := record.MayJudge(parsed, obj, m.Airlock.JudgeSeconds, neededADecision(requested)); err != nil {
				return Decision{}, false, fmt.Errorf("judge record %s: %w", recordID, err)
			}
		}
	} else {
		who.login, err = parsed.ApproverLogin(record.KindAgentSecret, "", who.login)
		if err != nil {
			return Decision{}, false, err
		}
	}
	// The request's names, read once rather than three times: the default lifetime below,
	// currentPolicyAdmits and decidedUnder all want them, and the requests row is locked
	// `for update` from the read above until this transaction ends, so three reads of them
	// could only ever answer the same rows.
	var requested []requestedSecret
	if approve {
		if requested, err = requestedSecrets(ctx, tx, requestID); err != nil {
			return Decision{}, false, err
		}
	}
	// A record that names no lifetime gets the default the current policy gives its names: a
	// request pending from before the 12-hour default, or one whose names were all
	// default-grant=session when it was made, is held to the policy as it stands at approval.
	lifetime := parsed.LifetimeSeconds
	if approve && lifetime == nil {
		names := make([]string, len(requested))
		for i, g := range requested {
			names[i] = g.name
		}
		lifetime = defaultLifetime(set, names, nil)
	}
	// denied is set when the current policy denies one of the names because the secret's
	// requesters tag no longer lists this session: the approval is recorded as the request's own
	// denial below rather than refused, since no login could approve such a name.
	var denied *policyDenial
	if approve {
		requester, err := m.requester(ctx, tx, enr, facts)
		if err != nil {
			return Decision{}, false, err
		}
		if err := currentPolicyAdmits(requested, set, requester, who, parsed.Approver, lifetime); err != nil && !errors.As(err, &denied) {
			return Decision{}, false, err
		}
	}
	switch {
	case state == "expired" || state == "pending" && expired:
		return Decision{}, false, ErrExpired
	case state != "pending":
		return Decision{}, false, ErrTerminal
	}
	if err := verifyObject(); err != nil {
		return Decision{}, false, err
	}
	// Who may decide a pending request is its hold's to say: while the judge holds it, the judge
	// alone, and no person from any page or route; once the hold has ended — handed over, or run
	// out — its person alone, and nothing the judge writes late.
	switch {
	case who.isJudge() && !held:
		return Decision{}, false, fmt.Errorf("decide record %s: %w", recordID, ErrJudgeHoldEnded)
	case !who.isJudge() && held:
		return Decision{}, false, fmt.Errorf("decide record %s: %w", recordID, ErrJudgeHolds)
	}

	event, by := "denied", who.actor()
	next, detail := "denied", who.refusal
	// decidedBy is what the requests row records as having decided it: the deciding login, the
	// judge's actor for the judge (D1), except where the policy rather than that decider made
	// the answer (PolicyDeniedActor).
	decidedBy := who.decidedBy()
	if approve {
		event = "approved"
		if mayGrant {
			next = "granted"
		} else {
			detail = "the requesting enrollment is no longer live"
		}
	}
	if denied != nil {
		event, by, next = "denied", PolicyDeniedActor, "denied"
		detail, decidedBy = denied.Error(), PolicyDeniedActor
	}
	if _, err := tx.Exec(ctx, `update requests set state=$2, decided_at=now(), decided_by=$3, decision_detail=$4,
		lifetime_seconds = case when $2 = 'granted' then $5::int else lifetime_seconds end where id=$1 and state='pending'`,
		requestID, next, decidedBy, nullable(detail), lifetime); err != nil {
		return Decision{}, false, err
	}
	if _, err := tx.Exec(ctx, `insert into credential_request_events (record_id, event, login, actor, detail) values ($1,$2,$3,$4,$5)`,
		recordID, event, who.eventLogin(), by, nullable(detail)); err != nil {
		return Decision{}, false, err
	}
	var grantID string
	if next == "granted" {
		if err := decidedUnder(ctx, tx, requestID, requested, set); err != nil {
			return Decision{}, false, err
		}
		// The grant lasts the lifetime the approved record names, or the default above.
		if grantID, err = insertGrant(ctx, tx, requestID, enr.ID, who.grantApprover(parsed.Approver), lifetime); err != nil {
			return Decision{}, false, err
		}
	}
	rowDetail := auditDetail{"detail": detail, "record_id": recordID}
	if err := audit(ctx, tx, "request."+next, enr.ID, requestID, nullable(grantID), by, rowDetail); err != nil {
		return Decision{}, false, err
	}
	// judged is the row the judge's verdict goes in as, built once here and logged below from
	// the same object, so the line and the row cannot say different things.
	var judged auditDetail
	if who.isJudge() {
		// D11: the judge's verdict goes in beside the decision, inside the decision's own
		// transaction, so the grant and the record of why it exists are written together or not
		// at all. Its actor is the judge even where the policy made the answer
		// (PolicyDeniedActor above): this row says what the judge decided, not what the request
		// became. Its applied, though, is what the request became: next, and nothing the judge
		// wrote (decider.judgedRow).
		judged = who.judgedRow(next)
		if err := audit(ctx, tx, judgedKind, enr.ID, requestID, nullable(grantID), who.actor(), judged); err != nil {
			return Decision{}, false, err
		}
	}
	if m.testHoldCommit != nil {
		if err := m.testHoldCommit(tx.Conn().PgConn().PID()); err != nil {
			return Decision{}, false, err
		}
	}
	// A COMMIT that failed may still have applied (its answer lost on the way back), so the
	// caller is told it cannot know from the error alone (ErrCommitUncertain).
	if err := tx.Commit(ctx); err != nil {
		return Decision{}, false, fmt.Errorf("%w: %w", ErrCommitUncertain, err)
	}
	if m.testCommitCut != nil {
		if err := m.testCommitCut(); err != nil {
			return Decision{}, false, fmt.Errorf("%w: %w", ErrCommitUncertain, err)
		}
	}
	// The service the policy verified for this request is on its own request.created row, and
	// its secrets are in request_secrets; neither is in hand here, so the line leaves both to
	// the route's read (grantLogEvent).
	ev := grantLogEvent("request."+next, by, enr)
	ev.RequestID, ev.GrantID, ev.State = requestID, grantID, next
	ev.Approver, ev.DecidedBy = parsed.Approver, decidedBy
	// lifetime_seconds is what the update above left on the row: the lifetime the grant was
	// given, and for every other answer the one the record froze, which is what the request row
	// has carried since it was written.
	rowLifetime := parsed.LifetimeSeconds
	if next == "granted" {
		rowLifetime = lifetime
	}
	if rowLifetime != nil {
		ev.LifetimeSeconds = *rowLifetime
	}
	ev.Reason, ev.Detail, ev.Raw = obj.Reason, detail, rawJSON(rowDetail)
	store.LogGrantEvent(ev)
	if who.isJudge() {
		logGrantRow(judgedKind, who.actor(), enr, requestID, grantID, rawJSON(judged))
	}
	return Decision{RequestID: requestID, State: next, GrantID: grantID, Detail: detail}, false, nil
}

// decidedUnder records, for each name of a request an approval is granting, the fingerprint of
// that name's airlock entry in set, the policy snapshot the approval was made against. A request
// freezes each name's fingerprint when it is created (insertRequest), which is the whole of it
// for a request the policy grants or denies at once; one that waits for a person — up to
// PendingTTL, half a day — can be approved across a rules roll, and what a release compares
// against must be the entry the approval was given under, else the grant the person just made
// ends on its first release with a reason that is not true (checkRelease's per-name comparison).
//
// The requests row's rules_version is left as the request was written under, and is not moved
// to this snapshot: it is what makes a release re-check the whole grant (checkRelease), and the
// approval of a name the current policy denies or no longer serves is admitted here on purpose
// — no login could put such a name back — for that very re-check to refuse at the first read.
// Moving it would skip the re-check for exactly those approvals. A version left behind only
// makes the re-check run more often than it must, and the fingerprint it compares is this one.
//
// One statement over every name of the request, by name, since every row's digest is read from
// the one snapshot: a name the rules do not name records "", as it does at creation. granted is
// the request's names, read once by the approve path that calls this.
func decidedUnder(ctx context.Context, tx pgx.Tx, requestID string, granted []requestedSecret, set *policy.Set) error {
	names := make([]string, len(granted))
	digests := make([]string, len(granted))
	for i, g := range granted {
		names[i], digests[i] = g.name, set.Secrets[g.name].AirlockDigest
	}
	_, err := tx.Exec(ctx, `update request_secrets s set airlock_digest = v.digest
		from unnest($2::text[], $3::text[]) as v(name, digest)
		where s.request_id = $1 and s.name = v.name`, requestID, names, digests)
	return err
}

// policyDenial is currentPolicyAdmits' answer for a name the current policy denies whoever
// decides: the secret's requesters tag no longer lists this session's person or its pod's service
// (policy.ReasonRequesterNotListed), the secret belongs to a private project the session does not
// name (or to one Dispatch does not hold), or Dispatch calls the session's person a restricted
// member (policy.ReasonRestrictedMember). It is deliberately not a record.ErrNotApprover: no login
// could approve such a name, so leaving the card pending would wait on a person who can do nothing
// with it. applyDecisionAttempt records the approval as the request's denial instead, and its
// message is that denial's detail, in the words a session is told (policy.CallerReason).
type policyDenial struct{ name, reason string }

func (e *policyDenial) Error() string {
	return e.name + ": " + e.reason
}

// PolicyDeniedActor is what a request the policy denied under an approval records as having
// decided it: requests.decided_by, the denied event's actor and the request.denied audit row's
// actor alike, since the policy and not the person deciding made that answer. The event keeps
// the approver's own login beside it, so the trail still names who pressed approve.
const PolicyDeniedActor = "broker:policy"

// currentPolicyAdmits refuses who — the person or the model judge deciding — an approval of a
// request when the current policy wants one of its names approved by someone else
// (decider.maySettle, which holds a person to record.MayDecide and the judge to recordApprover,
// the record's own approver), since a change of owner since the request was made moves who may
// approve it: a request waiting on anyone for a secret that is now
// a person's is that person's alone to approve, and one waiting on a person for a secret another
// person must now approve is no one's to approve (its record's approver may still deny it;
// otherwise it expires, or its session cancels it and asks again). requester carries the names the
// session's operator withheld from it, each human tier to the session, so a withheld name that the
// request got automatically before the withhold is its owner's to approve (anyone's, for a shared
// secret), whoever the record waits on. A denial releases nothing, so ApplyDecision does
// not ask. A name the current policy grants at once, denies or no longer serves names no
// approver, so it leaves the approval to the record's approver; Values refuses a grant of the last
// two at its first read. The one deny it answers for is a name the requesters tag no longer lists
// this session for (*policyDenial): no owner change could put it back in the session's reach, so
// ApplyDecision records the request denied rather than leaving a card no one can answer. That
// deny is read in a pass of its own over every name, ahead of every refusal below, because a
// request holding such a name has to be answered however its other names read: in one pass the
// first name that cannot be admitted wins, and both of the refusals below can come first — a
// name that is withheld as well as dropped from the list, and, in a request of several names, an
// earlier name the deciding login may not approve — each leaving on the approver's page a card
// that no person can ever act on, until it expires.
// A withheld name the policy denies for any other reason, or no longer serves, is refused
// instead: only its owner may approve it for the session, and no owner can be read while it is
// out of the policy, nor approves it while a service owns it (the one way left, once that pass
// has answered the list's own deny, for the policy to deny a withheld name).
// Admitted, such an approval would release the name once it returns with the tags it had, since
// that brings back the policy version the request was made under and Values re-checks the grant
// (stillAllowed) only when the version has moved. A secret whose max-grant the current policy sets
// below the request's lifetime, or that now has one the request names none for, is no one's to
// approve now, as with a withheld name: the session asks again within it.
//
// The model judge is held to two rules of its own, ahead of all of this: it settles no withheld
// name and no administrator's secret, however the policy reads that name now. The run made both
// checks itself before it asked a model anything (judge's withheld and admin checks), and then
// spent up to 20 seconds on the model call, so what either of them read could have changed in
// those seconds, and this transaction is the boundary where it is read again — every other check
// the run made is made again here or in record.MayJudge.
//
// A withheld name is hard to reach, since only names granted automatically are withheld and the
// name must have changed tier while the request waited. An admin secret is easier: its owner
// adds the tag while the model is answering, or a deployment writes a judge entry for a name
// that already carries it, and the judge's own check — made against the policy snapshot of a
// moment ago — would be the only thing refusing a credential whose tag promises that a person
// approves every release of it (policy.TagAdmin). That promise is the secret's and not the rules
// file's to make, so the author of the rules file cannot undo it here either. Both are one
// comparison to close.
//
// A person's path never reaches either guard: a withheld name a person may approve is still
// theirs to approve, and an admin secret is approved by the person its owner tag names, both on
// the rules below, which are the rules they have always been held to.
//
// set is the one snapshot the whole decision is made against, the one whose airlock fingerprints
// the request then records as what it was decided under (decidedUnder), and requested the
// request's names, read once by the approve path that calls this.
func currentPolicyAdmits(requested []requestedSecret, set *policy.Set, requester policy.Requester, who decider, recordApprover string, lifetime *int) error {
	if who.isJudge() {
		for _, g := range requested {
			if slices.Contains(requester.Withheld, g.name) {
				return fmt.Errorf("%w: %s is withheld from this session, and the judge settles no withheld name", record.ErrNotApprover, g.name)
			}
			if set.Secrets[g.name].Admin {
				return fmt.Errorf("%w: %s is an administrator's secret, and a person approves every release of one", record.ErrNotApprover, g.name)
			}
		}
	}
	for _, g := range requested {
		d, err := set.Evaluate(g.name, requester)
		if errors.Is(err, policy.ErrUnknownSecret) {
			continue
		}
		if err != nil {
			return err
		}
		if d.Outcome == policy.Deny && (d.Reason == policy.ReasonRequesterNotListed || d.Reason == policy.ReasonRestrictedMember || projectDeny(d)) {
			return &policyDenial{name: g.name, reason: policy.CallerReason(d.Reason)}
		}
	}
	for _, g := range requested {
		d, err := set.Evaluate(g.name, requester)
		if errors.Is(err, policy.ErrUnknownSecret) {
			if slices.Contains(requester.Withheld, g.name) {
				return fmt.Errorf("%w: %s is withheld from this session and the current policy does not serve it, so no one may approve it now", record.ErrNotApprover, g.name)
			}
			continue
		}
		if err != nil {
			return err
		}
		if d.Outcome == policy.Deny && slices.Contains(requester.Withheld, g.name) {
			return fmt.Errorf("%w: %s is withheld from this session and the current policy denies it, so no one may approve it now", record.ErrNotApprover, g.name)
		}
		if d.Outcome == policy.Approval && !who.maySettle(d.Approver, recordApprover) {
			return fmt.Errorf("%w: the current policy has %s approve %s", record.ErrNotApprover, d.Approver, g.name)
		}
	}
	for _, g := range requested {
		if limit, over := overMaxGrant(set, g.name, lifetime); over {
			return fmt.Errorf("%w: %w: %s now allows grants of at most %s, shorter than this request asks, so no one may approve it now; its session asks again within that bound", record.ErrNotApprover, ErrGrantRequiresLifetime, g.name, limit)
		}
	}
	return nil
}

// verifyRecordBody re-parses a credential_requests row's stored body and confirms it still hashes
// to its own id. The append-only trigger should make a mismatch unreachable outside a direct
// database tamper; ApplyDecision checks it before deciding a record rather than trusting the row
// blindly. VerifyChain's own re-verification goes through the shared record.ChainVerifier instead
// (m.Chain), which reproduces this same check as part of its larger chain.
func verifyRecordBody(recordID, body string) (record.Body, error) {
	parsed, err := record.VerifyBodyReproducesID(body, recordID)
	if err != nil {
		return record.Body{}, fmt.Errorf("%w: record %s: stored body does not reproduce its id", ErrGrantChainInvalid, recordID)
	}
	return parsed, nil
}

// VerifyChain re-verifies a grant's whole approval chain against whatever is true right now, not
// just what was true when the grant was minted, through the shared record.ChainVerifier
// machinery (m.Chain, built by NewChainVerifier): the record's stored body still hashes to its
// own id, its embedded request object still verifies, and it carries exactly one terminal
// decision, an approval by the login the record names as its approver — a tampered row, a forged
// grant with no approval event, an approval naming another login, or a second decision all fail
// here. An automatic grant (no record_id) needs none of this and always passes. A genuine
// dependency failure inside m.Chain.Verify (a Postgres error from FetchRecord or FetchDecisions)
// is returned as-is rather than folded into ErrGrantChainInvalid; only record.ErrChainBroken —
// every reason the chain itself does not verify — is wrapped. It is verifyChain without the
// record's id; the releases (checkRelease, for Values and AirlockValues) and reuseLiveGrant call
// verifyChainForRelease, which logs a chain that fails (GrantChainInvalidMessage).
func (m *Machine) VerifyChain(ctx context.Context, grantID string) error {
	_, err := m.verifyChain(ctx, grantID)
	return err
}

// verifyChain is VerifyChain's whole check, and also answers the id of the credential-request
// record the grant's chain rests on, so a release that logs a chain that fails names that record
// without reading the grant's row a second time. The id is "" for an automatic grant, which has
// no record and always passes, and for a grant whose row could not be read; it is the record's
// whenever the error is ErrGrantChainInvalid.
func (m *Machine) verifyChain(ctx context.Context, grantID string) (recordID string, err error) {
	var id *string
	if err := m.Store.Pool.QueryRow(ctx, `select r.record_id from grants g join requests r on r.id=g.request_id where g.id=$1`, grantID).Scan(&id); err != nil {
		return "", err
	}
	if id == nil {
		return "", nil
	}
	if _, err := m.Chain.Verify(ctx, *id); err != nil {
		if errors.Is(err, record.ErrChainBroken) {
			return *id, fmt.Errorf("%w: %s", ErrGrantChainInvalid, err)
		}
		return *id, err
	}
	return *id, nil
}

// RevokeGrant ends a grant a session holds — a session may end only its own grant. Revoking an
// already-revoked grant succeeds and changes nothing: it writes no second grant.revoked audit row,
// so the trail never names a revoker who ended nothing. Human revocation is RevokeByApprover.
// The owner check reads the grants row's own enrollment_id, which an adoption rewrites, so the
// moved session ends the grant that moved with it and every other enrollment is ErrNotYours. Before
// it, on the pool and since it may ask Dispatch, a session that does not hold the grant is answered
// as for a grant id no grant has, pgx.ErrNoRows, when the grant touches a private project it cannot
// see, and ErrProjectAccessUnavailable when Dispatch could not say (hideGrantFromSession): so
// ErrNotYours says only that a grant exists in a project the caller sees.
func (m *Machine) RevokeGrant(ctx context.Context, grantID, enrollmentID string) error {
	if err := m.hideGrantFromSession(ctx, grantID, enrollmentID); err != nil {
		return err
	}
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var owner, requestID string
	if err := tx.QueryRow(ctx, `select enrollment_id, request_id from grants where id=$1 for update`, grantID).Scan(&owner, &requestID); err != nil {
		return err
	}
	if owner != enrollmentID {
		return ErrNotYours
	}
	actor := "session:" + enrollmentID
	tag, err := tx.Exec(ctx, `update grants set revoked_at=now(), revoked_by=$2 where id=$1 and revoked_at is null`, grantID, actor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if err := audit(ctx, tx, "grant.revoked", owner, requestID, &grantID, actor, auditDetail{}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	logGrantRow("grant.revoked", actor, enrollmentRow{ID: owner}, requestID, grantID, rawJSON(auditDetail{}))
	return nil
}

// enrollment reads a live enrollment (not revoked, lease not lapsed); pgx.ErrNoRows otherwise.
func (m *Machine) enrollment(ctx context.Context, id string) (enrollmentRow, error) {
	var e enrollmentRow
	err := m.Store.Pool.QueryRow(ctx, `select e.id, e.kind, e.operator, c.service, c.host, e.subject, e.thumbprint, e.runtime_id, e.slot, coalesce(e.session_id,''), e.projects, coalesce(e.claim,'') from enrollments e
		left join launcher_credentials c on c.id = e.launcher_credential_id
		where e.id=$1 and e.revoked_at is null and enrollment_lease_live(e.lease_expires_at)`, id).
		Scan(&e.ID, &e.Kind, &e.Operator, &e.Service, &e.Host, &e.Subject, &e.Thumbprint, &e.RuntimeID, &e.Slot, &e.SessionID, &e.Projects, &e.Claim)
	return e, err
}

// lockLiveEnrollment takes enrollment id's row lock for share inside tx while the enrollment is
// live (not revoked, lease not lapsed); pgx.ErrNoRows when it is not. Taken before tx writes a
// request or grant row, the order ApplyDecision takes them in, it keeps endEnrollment off the row
// until tx commits: EndLapsed skips the locked row and Revoke waits for it, and either then
// cancels what tx wrote. An enrollment ended while tx waited for the lock is no longer live once
// Postgres rechecks the row, so tx writes nothing on it.
func lockLiveEnrollment(ctx context.Context, tx pgx.Tx, id string) error {
	var one int
	return tx.QueryRow(ctx, `select 1 from enrollments where id=$1 and revoked_at is null and enrollment_lease_live(lease_expires_at) for share`, id).Scan(&one)
}

// anyServiceOwned reports whether set serves any of granted as a service's secret, whose grant is
// re-checked on every use: who is a service's own session rests on BROKER_SERVICES' accounts,
// which the policy's version does not cover (policy.Set.ServiceOwned). The version does not cover
// BROKER_POD_OPERATORS either, which decides whom a person's pod acts for. personPodGrant opens
// the same re-check for each grant such a pod holds.
func anyServiceOwned(set *policy.Set, granted []requestedSecret) bool {
	return slices.ContainsFunc(granted, func(g requestedSecret) bool { return set.ServiceOwned(g.name) })
}

// anyListed reports whether set serves any of granted as a secret whose requesters tag says who
// may ask for it, whose grant is re-checked on every use: an entry naming a service names the
// pods whose account BROKER_SERVICES maps to it (Machine.requester), which the policy's version
// does not cover, so a pod can fall off a list the tag never changed. A list naming people alone
// is re-checked as well. For most sessions this answers what the version already decided, at the
// cost of one policy evaluation per name. For a person's pod it can answer otherwise, because
// BROKER_POD_OPERATORS decides whom such a pod acts for (personPodGrant). One predicate keeps the
// gate's two callers from telling the sorts apart.
func anyListed(set *policy.Set, granted []requestedSecret) bool {
	return slices.ContainsFunc(granted, func(g requestedSecret) bool { return set.Secrets[g.name].Requesters != "" })
}

// personPodGrant reports whether enr is a person's pod: a pod enrollment that carries an
// operator, which only a person's pod login makes. Each grant such a pod holds is re-checked on
// every use, whatever the policy's version. The person the pod acts for rests on
// BROKER_POD_OPERATORS (Machine.requester), which the policy's version does not cover. So when
// the setting drops the pod's account, or maps it to another person, the next release or reuse
// stops each grant the pod got only because it acted for that person.
func personPodGrant(enr enrollmentRow) bool {
	return enr.Kind == "pod" && enr.Operator != nil
}

// unmappedPersonPod reports whether enr is a person's pod (personPodGrant) that
// BROKER_POD_OPERATORS (PodOperators) no longer maps to its operator. The setting must map the
// account the enrollment recorded to that same person, both compared through
// record.CanonicalLogin. Otherwise the pod acts for no person. It is false for every other
// enrollment. requester and owner both read this one test, so the person a pod asks as and the
// person whose requests it reads and cancels cannot differ.
func (m *Machine) unmappedPersonPod(enr enrollmentRow) bool {
	if !personPodGrant(enr) {
		return false
	}
	var mapped string
	if enr.Subject != nil {
		mapped = record.CanonicalLogin(m.PodOperators[*enr.Subject])
	}
	return mapped == "" || mapped != record.CanonicalLogin(*enr.Operator)
}

// auditDetail is an audit row's non-secret detail object; audit writes it as JSON.
type auditDetail map[string]any

func audit(ctx context.Context, tx pgx.Tx, kind, enrollmentID, requestID string, grantID *string, actor string, detail auditDetail) error {
	_, err := tx.Exec(ctx, `insert into audit (kind, enrollment_id, request_id, grant_id, actor, detail) values ($1,$2,$3,$4,$5,$6)`, kind, enrollmentID, requestID, grantID, actor, detail)
	return err
}

// grantLogEvent is one audit row this package has just committed, as the grant-log line
// store.LogGrantEvent writes for it. The caller fills in what its own transaction already had in
// hand and nothing else: the enrollment's identity and person come from enr, and the caller adds
// the request, grant, state, decision and the row's own detail (Raw) beside them. A field no
// value is at hand for is left empty rather than read back with a second query — the service the
// broker verified for a request, which lives on that request's request.created row, and the
// secrets of a request a later transaction decides, among them. The audit table is the full
// record, and the grant-log route's read (store.ReadGrantLog) is what joins every one of those
// in for the quarterly review; this line is the copy the log pipeline keeps.
//
// It is called after the transaction has committed, never inside it, and once per audit row of a
// logged kind rather than once per transaction: a writer that ends several grants in one
// statement (withhold) writes one audit row per grant and so logs one line per grant. A request
// decision, a request.judged row the model judge wrote, and a grant's end are logged — never a
// grant.used row, of which one session writes one per release, and never an enrollment row,
// which internal/cipher/enroll owns — so the volume is one line per decision, one per judge run
// that reached a verdict, and one per grant end.
//
// A crash between the commit and the line loses the line and not the row.
func grantLogEvent(kind, actor string, enr enrollmentRow) store.GrantLogEvent {
	return store.GrantLogEvent{
		Kind:           kind,
		Actor:          actor,
		EnrollmentID:   enr.ID,
		Session:        store.GrantLogSession{Kind: enr.Kind, SessionID: enr.SessionID, RuntimeID: enr.RuntimeID},
		Person:         deref(enr.Operator),
		ClaimedService: deref(enr.Service),
	}
}

// logGrantRow logs the grant-log line of one audit row this package has just committed that
// names a grant or a judge's verdict: a grant.revoked or grant.withheld row, whose line names
// the grant, the request it was granted for and the row's own detail beside what grantLogEvent
// reads off enr, and nothing else, or a request.judged row, which names the same fields and
// leaves the grant empty when the verdict granted nothing. Every writer of such a row goes
// through it — a session's own revoke (RevokeGrant), a person's and each grant the withhold
// beside it ended (RevokeByApprover), the end a release writes when what was granted cannot come
// back (endGrant), and both writers of the judge's verdict (LogJudged and
// applyDecisionAttempt) — so they all build the same line. A request's own decision line sets
// fields past these three and builds its event itself.
func logGrantRow(kind, actor string, enr enrollmentRow, requestID, grantID string, raw json.RawMessage) {
	ev := grantLogEvent(kind, actor, enr)
	ev.RequestID, ev.GrantID, ev.Raw = requestID, grantID, raw
	store.LogGrantEvent(ev)
}

// endedRequestLogEvent is one request store.EndPendingRequests has just ended, as its grant-log
// line: the transition that statement wrote, who it recorded as deciding it (nobody, for an
// expiry the broker made) and the detail it gave the request, which is also what the route reads
// back off the request row. It takes the very store.RequestEnd its caller handed that statement,
// so the line and the row it stands for cannot drift. Its Raw is the audit row's detail as the
// statement wrote it: end.AuditDetail, and {}, the column's default, when that is nil, never null.
// Cancel, expirePending, denyUndecidable and Create's end of a request signed for another broker
// address (brokerMovedEnd, whose detail names its reason) all end requests through it.
func endedRequestLogEvent(end store.RequestEnd, enrollmentID, requestID string) store.GrantLogEvent {
	ev := grantLogEvent("request."+end.State, end.Actor, enrollmentRow{ID: enrollmentID})
	ev.RequestID, ev.State, ev.DecidedBy, ev.Detail = requestID, end.State, deref(end.DecidedBy), end.Detail
	detail := auditDetail(end.AuditDetail)
	if detail == nil {
		detail = auditDetail{}
	}
	ev.Raw = rawJSON(detail)
	return ev
}

// endedGrant is one grant withhold ended beside the revoke that triggered it, read back from the
// grant.revoked audit row withhold wrote for it, so RevokeByApprover logs that row without
// reading the table again.
type endedGrant struct {
	GrantID   string          `json:"grant"`
	RequestID string          `json:"request"`
	Detail    json.RawMessage `json:"detail"`
}

// rawJSON is an audit row's detail as the row holds it, for a grant-log line's Raw. A detail this
// package builds is strings and string slices, which always marshal; an object that somehow does
// not is left out of the line rather than failing the call that already committed.
func rawJSON(detail auditDetail) json.RawMessage {
	raw, err := json.Marshal(detail)
	if err != nil {
		return nil
	}
	return raw
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func sortedCopy(s []string) []string {
	sorted := append([]string(nil), s...)
	sort.Strings(sorted)
	return sorted
}
