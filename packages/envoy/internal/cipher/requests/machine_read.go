// packages/envoy/internal/cipher/requests/machine_read.go
// machine_read.go holds requests.Machine's read-model methods: queries the API's GET handlers
// and the value-release path use, as opposed to machine_state.go's write/decision/transition
// methods.
package requests

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// MintFailedMessage is logged, at ERROR, each time a type secret's minter fails a release,
// followed by name=<the secret's name>, type=<its type>, class=<secrets.MintErrorClass of the
// error>, attempts=<secrets.MintAttempts of the error: the attempts the upstream call the mint
// failed at made, 1 to 3, and 0 for a failure that was no call's> and error=<the minter's
// message>, which never holds the stored value or a token. The session's answer is 502
// MINT_FAILED; this line is the operator's.
const MintFailedMessage = "broker: mint failed"

// TrailingLineEndingRemovedMessage is logged, at INFO, in release, each time the line-ending rule
// (policy.TrimLineEnding) changed a value the broker read from the store, followed by
// name=<the secret's name> and nothing else: no grant, which release is given none of, and no
// part of any value. It is the operator's line for reading which secrets the rule reaches.
const TrailingLineEndingRemovedMessage = "broker: a stored value's trailing line ending was removed"

// NarrowingRefusedMessage is logged, at WARN, each time a narrowing of a GitHub App token is
// refused for what it names, which happens in two places. A release's refusal is one: an account
// a github-app secret of the grant does not list (403 NARROWING_OWNER_NOT_LISTED, before any
// minter runs), or GitHub's 422 to the narrowed token (422 NARROWING_REFUSED); that case carries
// grant=, enrollment=, name=<the secret's name> and error=. Create's check of a request's
// narrowing is the other (narrowing_check.go): every refusal it makes before anybody is asked to
// approve the request, which carries enrollment=, name= and error= and no grant= key at all,
// since no grant exists and the refusal is why none will. Neither case holds a value or a token
// in any key. A refused authorization the caller fixes, not a mint failure the operator acts on.
const NarrowingRefusedMessage = "broker: narrowing refused"

// PendingLeftOutMessage is logged, at WARN, once for each pending record PendingForApprover
// leaves out of an approver's list because its signed record does not verify for this broker:
// most often "aud does not match", from a request signed for another BROKER_PUBLIC_URL, after
// the public URL changed while it waited or when a second broker shares this database. It names
// the record id and the reason. Every other record of the list is listed and can be decided. An
// expired record a list that asks for states leaves out is not logged (PendingForApprover says
// why).
// A request whose record is signed for another broker address stays off every list here, and
// Create ends it on its session's next ask of this broker for one of its names that writes a
// request, pending or decided at once, or that a live grant answers, whatever its age beside a
// request of this broker (RequestEndedBrokerMovedMessage), while its deadline has not passed. An
// ask Create refuses before it writes a request ends nothing, and a request that no valid ask
// reaches before its deadline is expired by the sweep.
const PendingLeftOutMessage = "broker: a pending request's record does not verify for this broker; it is left out of the pending list"

// RequestEndedBrokerMovedMessage is logged, at INFO, once for each pending request Create ends
// because its record is signed for another broker address (signedForAnotherBroker): its session
// asked a broker of another BROKER_PUBLIC_URL that shares this database, or this broker before
// its public URL changed, and asks again here, where no person and no judge could decide the
// request it left waiting (PendingLeftOutMessage). It is written once the transaction that ended
// the request has committed, beside the request.cancelled grant-log line of the same end, so a
// Create that rolls back or whose commit fails writes none. It carries request=<the ended
// request's id>, record_id=<its credential-request record's id>, new_request=<the request the
// session is referred to now: the one Create made, granted, denied or pending, the oldest pending
// request of this broker it kept and coalesced onto or refused the ask with ErrRequestPending
// for, or, for an ask a live grant answered (reuseLiveGrant), the request of that grant> and
// enrollment=<the enrollment that asked>, and nothing else. Each is an id, so the line holds no
// secret value, no token and no request object.
const RequestEndedBrokerMovedMessage = "broker: a pending request's record is signed for another broker address; it is ended, and its session's new request replaces it"

// GrantChainInvalidMessage is logged, at WARN, each time a release finds that a grant's whole
// approval chain no longer verifies (ErrGrantChainInvalid, verifyChainForRelease), in one of three
// places, which at= names: values, the session's own release (Values), which the session is
// answered GRANT_CHAIN_INVALID for; airlock, the airlock proxy's read of the values it swaps in
// (AirlockValues), which the proxy answers the same; and reuse, Create's check of a live grant it
// would hand back for the same names (reuseLiveGrant), which passes that grant over for the next
// newest and, with none left, asks afresh. Most often the grant's record was signed for another
// BROKER_PUBLIC_URL ("aud does not match"): a broker of another address that shares this database
// made the grant, or this broker did before its public URL changed, and the grant's session asks
// here now, its enrollment having adopted the grant (enroll.go, adopt). So these lines count each
// release such a session was refused and each such grant its next ask passed over. It carries
// grant=<the grant's id>, record_id=<the credential-request record its chain rests on>,
// enrollment=<the enrollment that holds the grant: the grant's own>, at= and reason=<the error's
// text, which names the link of the chain that failed>, and nothing else: no secret value, no
// token and no request object. A database failure inside the chain check (verifyChain: reading
// the grant, its record or its decisions) writes no such line, since it is no failed chain, and
// the release answers that error as it is. ApplyDecision's refusal of a record whose chain is
// broken is a decision and no release, and writes none either.
const GrantChainInvalidMessage = "broker: a grant's approval chain does not verify; it releases nothing"

// GrantApproverLapsedMessage is logged, at WARN, each time a release or a reuse refuses a grant a
// member of a group approver decided because the record's group does not admit that decider now
// (record.ApproverNotAdmittedError, which ChainVerifier.Verify alone answers; noteLapsedApprover):
// the group is stale or was never read, a lapsed grant, which writes no GrantChainInvalidMessage
// beside it; or the group is fresh and does not list the decider, which is a broken chain too and
// writes that line beside this one. It carries grant=, record_id=, secret=<the grant's names,
// comma-joined>, decider=<the login that decided>, group=<the group's address>, reason=decider is
// not a member now and at= (values, airlock or reuse), and nothing else: no value and no token.
const GrantApproverLapsedMessage = "group approval lapsed"

// noteLapsedApprover writes GrantApproverLapsedMessage for grant grantID, whose chain rests on
// recordID and which holds granted, refused at at, when err holds a
// record.ApproverNotAdmittedError, fresh or not, and writes nothing for any other error.
func noteLapsedApprover(grantID, recordID, at string, granted []requestedSecret, err error) {
	var lapsed record.ApproverNotAdmittedError
	if !errors.As(err, &lapsed) {
		return
	}
	names := make([]string, len(granted))
	for i, g := range granted {
		names[i] = g.name
	}
	slog.Warn(GrantApproverLapsedMessage, "grant", grantID, "record_id", recordID, "secret", strings.Join(names, ","),
		"decider", lapsed.Login, "group", lapsed.Group, "reason", "decider is not a member now", "at", at)
}

// Release is one release of a grant: the values the session may hold, when the grant expires,
// and, when the grant holds a name behind the airlock, how to reach the proxy that holds the
// rest.
type Release struct {
	Values  map[string]string
	Expires *time.Time
	Airlock *AirlockRelease // nil when no granted name is behind the airlock
}

// grantRelease is a live grant as checkRelease found it: the enrollment that holds it, the
// request it came of and the reason that request gave, when it expires, the names it was
// granted, and the policy snapshot its re-checks were made against, which the reads that follow
// mint from.
type grantRelease struct {
	enrollmentID, requestID, reason string
	expires                         *time.Time
	granted                         []requestedSecret
	set                             *policy.Set
	// facts are what Dispatch answered for this release, kept so the airlock's own reads
	// (channelLimit) ask it nothing a second time; enr is the enrollment they were read for, so
	// channelLimit can read them again once they aged past the bound.
	facts projectFacts
	enr   enrollmentRow
}

// checkRelease is every check a release of a grant makes before it reads a value, shared by
// Values and AirlockValues so that what the broker proxies and what it hands over are held to
// one rule. It re-checks the enrollment and the grant, re-verifies the grant's whole approval
// chain (verifyChainForRelease, which logs a chain that fails, and noteLapsedApprover, which logs
// a group approval whose decider the group does not admit now), and — when the policy has
// changed since the grant's request was decided, any granted name is a service's
// (anyServiceOwned: the policy's version does not cover which service BROKER_SERVICES' accounts
// prove), any granted name carries a requesters tag (anyListed: an entry naming a service
// matches by those same accounts, and one naming a group by the group's members), any granted
// name is a group's (anyGroupOwned: the version does not cover the group's members), or the
// grant is a person's pod's (personPodGrant: the policy's version does not cover which person
// BROKER_POD_OPERATORS maps the pod's account to) — that the current policy still allows the
// session, with the names its operator withheld from it, every granted name under the login that
// decided it, or, for a grant the model judge decided, under the judge the rules still give that
// name (stillAllowed), that no granted name's max-grant is now shorter than the lifetime the
// grant's request signed (overMaxGrant), and that no granted name's airlock entry has changed
// since the request recorded its fingerprint, which ends the grant for good.
//
// For a person's pod, the session is the pod as requester reads it now. When the setting no
// longer maps the pod's account to its person, an automatic grant the pod got as that person is
// refused (ErrGrantNotLive) but not ended, so it releases again when the entry comes back. A
// grant a person approved still releases. A grant whose requesters tag no longer names the person
// ends for good (errRequesterNotListed); one whose requesters tag names a group the broker has not
// read in the last two hours is refused and not ended (policy.ReasonRequesterGroupUnread), and
// releases again once a read lists the person.
//
// owner, when it is not empty, is the enrollment the caller proved, which must be the grant's own
// (ErrNotYours). AirlockValues passes none: a proxy connection proves its grant by holding one of
// that grant's placeholders, which AirlockConnection checked, and the session it belongs to is
// what the row answers rather than what the caller claims.
//
// at is the release GrantChainInvalidMessage names when the chain fails: "values" from Values and
// "airlock" from AirlockValues. The line's enrollment is the grant's own, read here, which is the
// owner the caller proved wherever it proved one.
func (m *Machine) checkRelease(ctx context.Context, grantID, owner, at string) (grantRelease, error) {
	var enrollmentID, requestID, reason, policyVersion, decidedBy string
	var expires *time.Time
	var lifetime *int
	var live bool
	var enr enrollmentRow
	err := m.Store.Pool.QueryRow(ctx, `select g.enrollment_id, g.expires_at, g.revoked_at is null and `+grantUnexpired("g")+` and e.revoked_at is null and enrollment_lease_live(e.lease_expires_at),
		g.request_id, r.reason, r.rules_version, coalesce(r.decided_by, ''), r.lifetime_seconds, e.kind, e.operator, e.subject, c.service, c.host, e.projects
		from grants g join enrollments e on e.id=g.enrollment_id join requests r on r.id=g.request_id
		left join launcher_credentials c on c.id = e.launcher_credential_id where g.id=$1`, grantID).
		Scan(&enrollmentID, &expires, &live, &requestID, &reason, &policyVersion, &decidedBy, &lifetime, &enr.Kind, &enr.Operator, &enr.Subject, &enr.Service, &enr.Host, &enr.Projects)
	if errors.Is(err, pgx.ErrNoRows) {
		return grantRelease{}, ErrGrantNotLive
	}
	if err != nil {
		return grantRelease{}, err
	}
	enr.ID = enrollmentID
	if owner != "" && owner != enrollmentID {
		return grantRelease{}, ErrNotYours
	}
	if !live {
		return grantRelease{}, ErrGrantNotLive
	}
	granted, err := requestedSecrets(ctx, m.Store.Pool, requestID)
	if err != nil {
		return grantRelease{}, err
	}
	if recordID, err := m.verifyChainForRelease(ctx, grantID, enrollmentID, at); err != nil {
		noteLapsedApprover(grantID, recordID, at, granted, err)
		return grantRelease{}, err
	}
	// The grant's names, once the grant is the caller's and its chain verifies, so a refusal of
	// the release from here on names them (answer.Recorder.NoteSecrets).
	recorder := answer.FromContext(ctx)
	for _, g := range granted {
		recorder.NoteSecrets(g.name)
	}
	// One policy snapshot serves both the grant re-check and the minters' name, tier and owners.
	// A grant of a project-tagged name is re-checked on every release as a service's is: whether
	// its project is private, and whether the session's person may still see it, are Dispatch's
	// and not in the policy version (anyProjectTagged), so they are read here, on the pool and
	// outside any transaction (projectFacts), before the names are re-evaluated. A grant holding
	// a project-channels name is read every time too (anyChannelLimited): which channels its
	// credential may post to is Dispatch's as well, and the airlock hands the pod that list.
	set := m.Policy.Get()
	names := make([]string, len(granted))
	for i, g := range granted {
		names[i] = g.name
	}
	var facts projectFacts
	if policyVersion != set.Version || anyServiceOwned(set, granted) || anyListed(set, granted) || anyGroupOwned(set, granted) || anyProjectTagged(set, granted) || anyChannelLimited(set, names) || personPodGrant(enr) {
		// The session's own rows are read before Dispatch is asked, so that nothing stands between
		// Dispatch's answers and the rule that decides on them: projectFacts answers facts younger
		// than projectaccess.CacheTTL as it returns (atDecision), and they are decided on then.
		own, err := m.requester(ctx, m.Store.Pool, enr, projectFacts{})
		if err != nil {
			return grantRelease{}, err
		}
		if facts, err = m.projectFacts(ctx, enr, names, set); err != nil {
			return grantRelease{}, err
		}
		requester := facts.requester(own)
		for _, g := range granted {
			if err := stillAllowed(set, m.Airlock, g.name, g.source, g.decision, decidedBy, lifetime, requester); err != nil {
				// A grant whose frozen source is not the one the name is served from now (its type
				// was changed or removed) ends for good: setting the type back serves the same
				// source again, and the grant must not come back with it. So does one whose
				// requesters tag no longer lists this session: listing the person again must not
				// revive a grant made while they were off the list. So does a judged grant of a
				// secret the policy now tags as an administrator's: a person approves every
				// release of one, so taking the tag off again must not revive a grant no person
				// ever approved — the session asks again, as a new request.
				if errors.Is(err, errSourceMoved) || errors.Is(err, errRequesterNotListed) || errors.Is(err, errJudgedAdminSecret) || errors.Is(err, errProjectNotNamed) {
					return grantRelease{}, m.endGrant(ctx, grantID, err)
				}
				return grantRelease{}, err
			}
			// A max-grant added or lowered since the request was decided ends this grant for
			// good, as a raise later must not bring it back: the version covers the tag, so this
			// runs whenever it moved.
			if limit, over := overMaxGrant(set, g.name, lifetime); over {
				return grantRelease{}, m.endGrant(ctx, grantID, fmt.Errorf("%w: %w: %s now allows grants of at most %s", ErrGrantNotLive, ErrGrantRequiresLifetime, g.name, limit))
			}
			// A name whose airlock entry is not the one its request recorded ends this grant
			// for good: what was granted was decided under the old entry, so putting the entry
			// back does not bring the grant back. The comparison is per name, so only the
			// grants an entry's change affects end. It sits inside this block because an
			// entry's change moves the policy version (policy.NewSet folds the fingerprint
			// in), so the block runs whenever any entry changed and the common path pays
			// nothing for this. During a roll the two pods may read different files for a
			// moment; a grant the one with the newer file ends is simply asked for again.
			if held := set.Secrets[g.name].AirlockDigest; held != g.airlockDigest {
				return grantRelease{}, m.endGrant(ctx, grantID, fmt.Errorf("%w: %s's airlock entry changed since this grant was decided", ErrGrantNotLive, g.name))
			}
		}
	}
	return grantRelease{enrollmentID: enrollmentID, requestID: requestID, reason: reason, expires: expires, granted: granted, set: set, facts: facts, enr: enr}, nil
}

// verifyChainForRelease is verifyChain for a release of grantID, which enrollmentID holds: the
// session's (Values) and the airlock proxy's (AirlockValues), both through checkRelease, and
// Create's reuse of a live grant (reuseLiveGrant). A chain that fails, ErrGrantChainInvalid and
// only that, writes GrantChainInvalidMessage naming at, the release, with the error's text as its
// reason; any other error, a database failure inside the check among them, writes nothing, since
// it is no failed chain. The error is returned as verifyChain answered it, beside the id of the
// record the chain rests on, so each caller refuses or passes over exactly as before the line.
// ApplyDecision, which also answers ErrGrantChainInvalid, is a decision and no release, and does
// not come through here.
func (m *Machine) verifyChainForRelease(ctx context.Context, grantID, enrollmentID, at string) (string, error) {
	recordID, err := m.verifyChain(ctx, grantID)
	if errors.Is(err, ErrGrantChainInvalid) {
		slog.Warn(GrantChainInvalidMessage, "grant", grantID, "record_id", recordID, "enrollment", enrollmentID, "at", at, "reason", err.Error())
	}
	return recordID, err
}

// Values releases the values of a live grant to its own enrollment, and when the grant expires:
// nil for a grant that lasts until its session ends. Every call runs checkRelease's whole set of
// re-checks and then the release loop both releases of a grant run (readRelease, release.go). A
// stored secret releases its frozen source. A type secret reads that same stored source and
// passes its value to the type's minter. It holds no pooled connection across a Secrets Manager
// read: the grant's names are read into memory before the first value is fetched. A non-nil
// narrowing is this release's alone: it reaches the github-app minter of every github-app name
// the grant holds, and the grant records nothing of it, so its next release without one is
// owner-wide again. A grant that holds no github-app name refuses a narrowing
// (ErrNarrowingNotApplicable), one whose github-app name does not list the narrowing's owner in
// the snapshot the release mints from refuses it (ErrNarrowingOwnerNotListed), and so does one
// any of whose granted names is behind the airlock (ErrNarrowingBehindAirlock, narrowingRefusal);
// each releases nothing and runs no minter.
//
// A granted name the airlock's rules name is released as placeholders instead of as its value:
// its value is read or minted exactly as every other name's is, which is what says the value
// exists and which slots it fills, and is then discarded, a minted one given back to its upstream
// once the release is recorded (giveBack, heldBack), since the proxy mints a value of its own
// (AirlockValues). Such a name is left out of Values altogether — a CLI from before the airlock
// finds it missing and runs nothing, rather than running the command with a placeholder it does
// not understand — and what the session needs to reach it is Release.Airlock.
//
// call is what the session says of this release: the command it starts and the reason its own
// call gave. The release records both, with the reason of the grant's request, in its grant.used
// row and its ReleasedMessage line (auditGrantUsed); nothing else reads them, and they decide
// nothing about the release.
//
// Before checkRelease's owner check, a session that does not hold the grant is refused as for a
// grant id no grant has, ErrGrantNotLive and only that, when the grant touches a private project
// it cannot see, and ErrProjectAccessUnavailable when Dispatch could not say
// (hideGrantFromSession): so ErrNotYours says only that a grant exists in a project the caller
// sees. The session that holds the grant is checkRelease's alone, which ends a grant of a private
// project the session no longer names for good.
func (m *Machine) Values(ctx context.Context, grantID, enrollmentID string, narrowing *secrets.GitHubNarrowing, call ReleaseCall) (Release, error) {
	if err := m.hideGrantFromSession(ctx, grantID, enrollmentID); errors.Is(err, pgx.ErrNoRows) {
		return Release{}, ErrGrantNotLive
	} else if err != nil {
		return Release{}, err
	}
	checked, err := m.checkRelease(ctx, grantID, enrollmentID, "values")
	if err != nil {
		return Release{}, err
	}
	if narrowing != nil {
		if name, err := m.narrowingRefusal(checked.set, checked.granted, narrowing); err != nil {
			if errors.Is(err, ErrNarrowingOwnerNotListed) {
				slog.Warn(NarrowingRefusedMessage, "grant", grantID, "enrollment", enrollmentID, "name", name, "error", err)
			}
			return Release{}, err
		}
	}
	read, err := m.readRelease(ctx, grantID, checked, releaseRead{narrowing: narrowing})
	if err != nil {
		return Release{}, err
	}
	values := map[string]string{}
	splits := map[string]airlockSplit{}
	for _, released := range read {
		if released.behindAirlock() {
			// The real value goes no further than the loop that read it: the session is given
			// the placeholders airlockRelease mints for the slots the split found.
			splits[released.name] = released.split
			continue
		}
		values[released.name] = released.value
	}
	release := Release{Values: values, Expires: checked.expires}
	if len(splits) > 0 {
		if release.Airlock, err = m.airlockRelease(ctx, grantID, checked.expires, splits); err != nil {
			m.giveBack(ctx, read)
			return Release{}, err
		}
	}
	// A release with no command, every release from a client from before the field among them,
	// collects and parses nothing for a redaction it would not make.
	command := []string{}
	if len(call.Command) > 0 {
		command = loggedCommand(call.Command, handedOut(read, release.Airlock))
	}
	use := grantUse{names: releasedNames(read), command: command, callReason: callReasonOf(call.Reason, checked.reason), narrowing: narrowing}
	if err := m.auditGrantUsed(ctx, grantID, checked, use); err != nil {
		// The row is what says the values left the broker, and a release whose row failed hands
		// nothing over, so what it minted is given back.
		m.giveBack(ctx, read)
		return Release{}, err
	}
	m.giveBack(ctx, heldBack(read))
	return release, nil
}

// readValue is one granted name's value and, for a minted type, when that value stops working,
// with the refusals every release answers: a name the
// store no longer holds (ErrSecretNotInStore), a narrowing the minter refused, which is logged as
// a refused narrowing and handed back as it is, and any other failure named with the secret.
func (m *Machine) readValue(ctx context.Context, grantID, enrollmentID string, g requestedSecret, served policy.Secret, narrowing *secrets.GitHubNarrowing) (secrets.Minted, error) {
	minted, err := m.release(ctx, g, served, narrowing)
	if errors.Is(err, secrets.ErrNotFound) {
		return secrets.Minted{}, fmt.Errorf("%w: %s", ErrSecretNotInStore, g.name)
	}
	if errors.Is(err, secrets.ErrMintNarrowingRefused) {
		slog.Warn(NarrowingRefusedMessage, "grant", grantID, "enrollment", enrollmentID, "name", g.name, "error", err)
	}
	if err != nil {
		return secrets.Minted{}, fmt.Errorf("read %s: %w", g.name, err)
	}
	return minted, nil
}

// narrowingRefusal is why narrowing cannot reach granted, with the name it refuses for, in the
// order the three are reported: the grant holds no github-app secret
// (ErrNarrowingNotApplicable), a github-app secret of it does not list the narrowing's owner in
// its github-owners tag in set, the snapshot the names are judged against
// (ErrNarrowingOwnerNotListed), or the airlock holds back one of those names
// (ErrNarrowingBehindAirlock, naming the first such name). GitHubApp.Mint refuses that owner
// too, as a second barrier.
//
// Both places a narrowing is judged make these three refusals through this one function, so
// neither can answer one of them differently: the release of a grant (Values, where set is the
// snapshot the release mints from and granted its granted names) and Create's check before
// anybody is asked (checkNarrowing, where set is the snapshot the request is evaluated against
// and granted the names it asks for).
//
// The airlock's refusal is there because the broker would answer that it applied the narrowing
// while the proxy went on swapping in the value it holds for that grant, which is minted
// owner-wide. Recording the narrowing with the grant's placeholders instead was the other way
// out, and is not taken: a narrowing is one release's alone — the grant records nothing of it,
// and its next release without one is owner-wide again (Values) — while the proxy mints per pod
// for the life of the grant, so recording it there would make a per-release narrowing stick to
// the grant, which is a different promise from the one the CLI's help makes.
func (m *Machine) narrowingRefusal(set *policy.Set, granted []requestedSecret, narrowing *secrets.GitHubNarrowing) (string, error) {
	held := false
	for _, g := range granted {
		if secretType, _, minted, err := policy.ParseTypeSource(g.source); err != nil || !minted || secretType != policy.TypeGitHubApp {
			continue
		}
		held = true
		if !slices.Contains(set.Secrets[g.name].GitHubOwnerLogins(), narrowing.Owner) {
			return g.name, fmt.Errorf("%w: %s's github-owners tag does not list %s", ErrNarrowingOwnerNotListed, g.name, narrowing.Owner)
		}
	}
	if !held {
		return "", ErrNarrowingNotApplicable
	}
	for _, g := range granted {
		if m.Airlock.Bound(g.name) != nil {
			return g.name, fmt.Errorf("%w: the airlock holds %s, whose value the proxy spends for this grant", ErrNarrowingBehindAirlock, g.name)
		}
	}
	return "", nil
}

// recheckStoredSources checks every name of grantID that releases its stored value (a source no
// type froze) against the policy as it stands after Values read the store, since the policy Values
// checked before its reads can be replaced while they run: a retag to a type, reread as a read
// starts, makes the read return what the secret stores as that type (a GitHub App key, or a Google
// refresh token stored once the reread served the type). A name now served from another source,
// a type's included, ends the grant for good (endGrant), as it does before the reads; a name the
// policy no longer serves pauses it, as before the reads. Either way Values releases none of the
// values it read. A minted name needs no such check: its minter returns only a token.
func (m *Machine) recheckStoredSources(ctx context.Context, grantID string, granted []requestedSecret) error {
	now := m.Policy.Get()
	for _, g := range granted {
		if _, _, minted, _ := policy.ParseTypeSource(g.source); minted {
			continue
		}
		current, served := now.Secrets[g.name]
		if !served {
			return noLongerServed(g.name)
		}
		if current.Source != g.source {
			return m.endGrant(ctx, grantID, sourceMoved(g.name))
		}
	}
	return nil
}

// noLongerServed is the refusal of a granted name the current policy does not serve: a pause,
// since a value stored later serves it again.
func noLongerServed(name string) error {
	return fmt.Errorf("%w: %s is no longer an agent secret", ErrGrantNotLive, name)
}

// sourceMoved is the refusal of a granted name the current policy serves from another source than
// its grant froze, which ends the grant for good (errSourceMoved).
func sourceMoved(name string) error {
	return fmt.Errorf("%w: %w: %s is now served from another source than this grant's", ErrGrantNotLive, errSourceMoved, name)
}

// requesterNotListed is the refusal of a granted name whose requesters tag no longer names the
// session's person or its pod's service, which ends the grant for good (errRequesterNotListed).
func requesterNotListed(name string) error {
	return fmt.Errorf("%w: %w: %s no longer lists this requester", ErrGrantNotLive, errRequesterNotListed, name)
}

// judgedAdminSecret is the refusal of a granted name the live policy tags as an administrator's
// (policy.TagAdmin) on a grant the model judge decided, which ends the grant for good
// (errJudgedAdminSecret).
func judgedAdminSecret(name string) error {
	return fmt.Errorf("%w: %w: %s is an administrator's secret or an Anthropic federation secret, and a person approves every release of one", ErrGrantNotLive, errJudgedAdminSecret, name)
}

// GrantEndedActor is what a grant the broker itself ended at release records as having ended it,
// as revoked_by and as its grant.revoked audit row's actor.
const GrantEndedActor = "broker:release-recheck"

// endGrant ends grantID for good in one transaction, revoking it and writing a grant.revoked audit
// row naming GrantEndedActor and why (reason's text), then answers reason, an ErrGrantNotLive. A
// release ends a grant this way when what it was granted cannot come back: its frozen source is
// not the one served now (a type change), its lifetime is longer than the secret's max-grant
// now, or the name's airlock entry is not the one the request recorded. A secret the policy
// merely stops serving (no value, a malformed tag) pauses its grants instead, which release
// again once it is served. A grant another writer revoked first is left as it is, with no second
// audit row.
func (m *Machine) endGrant(ctx context.Context, grantID string, reason error) error {
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var enrollmentID, requestID string
	err = tx.QueryRow(ctx, `update grants set revoked_at=now(), revoked_by=$2 where id=$1 and revoked_at is null returning enrollment_id, request_id`, grantID, GrantEndedActor).
		Scan(&enrollmentID, &requestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reason
	}
	if err != nil {
		return err
	}
	rowDetail := auditDetail{"reason": reason.Error()}
	if err := audit(ctx, tx, "grant.revoked", enrollmentID, requestID, &grantID, GrantEndedActor, rowDetail); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// The grant's own release path reads no enrollment row here, so the line names the
	// enrollment and nothing else of the session; the route's read fills the rest in
	// (grantLogEvent).
	logGrantRow("grant.revoked", GrantEndedActor, enrollmentRow{ID: enrollmentID}, requestID, grantID, rawJSON(rowDetail))
	return reason
}

// release is one granted name's value, and when that value stops working for a type whose
// minter says. A stored secret reads its frozen source and ignores the
// policy metadata and narrowing; it carries no expiry, since nothing but a new stored version
// ends it. A type source reads its frozen store source (on Secret
// Manager its deletion verdict is cached for 30 s, as for every secret), then passes that value
// and the name, tier, GitHub owners and Google OAuth scopes from the policy snapshot Values
// checked to the minter. Either value is spent by the one line-ending rule
// (policy.TrimLineEnding): the stored value a session is handed, and the input the minter is
// given, are read by the one rule, and each value the rule changed is logged as
// TrailingLineEndingRemovedMessage with the secret's name. A token a minter makes is never
// touched; the rule reaches the stored value alone.
// An Azure login also requires this name's grant decision to be approval, never automatic.
// Only the github-app minter reads narrowing. A minter's refusal of the narrowing itself
// (secrets.ErrMintNarrowingRefused) is returned as it is,
// for Values to log as a refused narrowing, and is no mint failure. A minter that refuses the
// stored value itself (secrets.ErrMintStoredValue, its secrets.ParseValue) also has the policy
// refuse the secret as value-malformed (policy.Current.RefuseValue), at the version the live
// policy listed when this release read the value and under the type it was decoded
// under, so the grants of it pause until a new value is stored; a retag since leaves the secret
// to the decode of its new type. The failure line and this release's answer are every
// other mint failure's. A deleted secret stops releasing once the policy drops it:
// `agent-secrets secret delete` rereads the name at once.
func (m *Machine) release(ctx context.Context, g requestedSecret, served policy.Secret, narrowing *secrets.GitHubNarrowing) (secrets.Minted, error) {
	secretType, source, minted, err := policy.ParseTypeSource(g.source)
	if err != nil {
		return secrets.Minted{}, fmt.Errorf("secret %s: %w", g.name, err)
	}
	if !minted {
		stored, err := m.Secrets.Read(ctx, source)
		if err != nil {
			return secrets.Minted{}, err
		}
		return secrets.Minted{Value: trimmedValue(stored, g.name)}, nil
	}
	if secretType == policy.TypeAzureLogin && g.decision != policy.Approval {
		return secrets.Minted{}, fmt.Errorf("%w: %w: Azure login requires a person's approval", ErrMintFailed, secrets.ErrMintRefused)
	}
	mint := m.Minters[secretType]
	if mint == nil {
		return secrets.Minted{}, fmt.Errorf("secret %s is a minted type %q, but this broker has no token minter for it", g.name, secretType)
	}
	// The version the policy lists for the value this release reads, which a refusal of that
	// value below is keyed on: a load that lists a newer one meanwhile decodes that one itself.
	version := m.Policy.Get().ValueVersion(g.name)
	value, err := m.Secrets.Read(ctx, source)
	if err != nil {
		return secrets.Minted{}, err
	}
	token, err := mint(ctx, mintRequest(g.name, secretType, served, trimmedValue(value, g.name), narrowing))
	if errors.Is(err, secrets.ErrMintNarrowingRefused) {
		return secrets.Minted{}, err
	}
	if err != nil {
		// The minter's error names its class and why, never the stored value or a token, so the
		// line is safe to log; the session sees the same words as MINT_FAILED.
		slog.Error(MintFailedMessage, "name", g.name, "type", secretType, "class", secrets.MintErrorClass(err), "attempts", secrets.MintAttempts(err), "error", err)
		if errors.Is(err, secrets.ErrMintStoredValue) {
			m.Policy.RefuseValue(ctx, g.name, version, secretType)
		}
		return secrets.Minted{}, fmt.Errorf("%w: %w", ErrMintFailed, err)
	}
	return token, nil
}

// mintRequest is the input one granted name's release gives its minter: the name, the type tag
// its minters count their attempts under, the stored value as the line-ending rule spends it, the
// tier, GitHub owners and Google OAuth scopes of the policy snapshot the release was checked
// against, and the narrowing the caller asked for, nil where it asked for none. Create's check
// builds its request here too and sets Check on it (checkNarrowingMint), which is the whole
// worth of the check: it asks GitHub about the same secret, with the same owners and the same
// narrowing, as the release it stands for.
func mintRequest(name, secretType string, served policy.Secret, value string, narrowing *secrets.GitHubNarrowing) secrets.MintRequest {
	return secrets.MintRequest{
		Name:            name,
		Type:            secretType,
		HumanTier:       served.Tier == policy.TierHuman,
		Value:           value,
		GitHubOwners:    served.GitHubOwnerLogins(),
		GoogleScopes:    served.ScopeURLs(),
		GitHubNarrowing: narrowing,
	}
}

// trimmedValue answers the value release spends for a stored value it has just read
// (policy.TrimLineEnding), and writes the operator's line
// (TrailingLineEndingRemovedMessage) naming the secret each time the rule changed it. Both of
// release's reads pass through it, so the value a session is handed and the input a minter is
// given are read by the one rule and logged alike.
func trimmedValue(value, name string) string {
	spend, changed := policy.TrimLineEnding(value)
	if changed {
		slog.Info(TrailingLineEndingRemovedMessage, "name", name)
	}
	return spend
}

// requestedSecret is one name a request asked for that the policy did not deny when the request
// was made: the secret it froze (source), the decision the policy gave it then, and the
// fingerprint of its airlock entry then (airlockDigest, "" for a name the rules do not name).
type requestedSecret struct{ name, source, decision, airlockDigest string }

// requestedSecrets reads request requestID's names that the policy did not deny, in name order:
// what a granted request's grant releases (Values, reuseLiveGrant), and what a pending request
// waits to have approved (currentPolicyAdmits), since one denied name denies the whole request.
func requestedSecrets(ctx context.Context, q querier, requestID string) ([]requestedSecret, error) {
	rows, err := q.Query(ctx, `select name, source, decision, airlock_digest from request_secrets where request_id=$1 and decision <> 'deny' order by name`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var requested []requestedSecret
	for rows.Next() {
		var g requestedSecret
		if err := rows.Scan(&g.name, &g.source, &g.decision, &g.airlockDigest); err != nil {
			return nil, err
		}
		requested = append(requested, g)
	}
	return requested, rows.Err()
}

// neededADecision answers, for a request's names as requestedSecrets reads them, which ones the
// policy wanted approved when the request was made: the keys the judge decided of a request it
// held, where every other name is one the session got at once (record.MayJudge).
func neededADecision(requested []requestedSecret) func(string) bool {
	return func(name string) bool {
		return slices.ContainsFunc(requested, func(g requestedSecret) bool { return g.name == name && g.decision == policy.Approval })
	}
}

// stillAllowed re-checks one granted name against a policy newer than the one its request was
// decided under, or against the group reader's members now for a group's name. The policy must
// still serve the name and still let this requester have it: a deny refuses; so does a name
// granted automatically that the current policy wants approved, since no human ever approved it;
// and so does a name the current policy wants approved by someone decidedBy, the login that
// decided the request, is not (record.MayDecide, with the set's group reader). A group-approved
// record whose group does not admit its decider now is the chain check's to refuse before this
// runs (record.ChainVerifier.Verify, noteLapsedApprover), so the refusal here is the plain one. So
// an approval keeps its grant while the secret stays the approver's, or becomes shared, whose
// approver is anyone, or once the policy gives this requester the secret without asking; and a
// secret handed to a person stops every grant of it that person must approve and did not. A name
// withheld from the session that its request got automatically is judged as an approval by
// decidedBy too: the withhold ended every grant that got the name without asking (withhold), so a
// live one was approved after it, by someone the withheld name let approve it
// (currentPolicyAdmits), and an automatic grant, which no one decided, never passes. A grant whose
// frozen source is not the one the policy now serves the name from (the broker moved between
// stores, or, on AWS, the secret was replaced by another of the same name, which gets a new ARN;
// a Secret Manager source is the resource name, which a recreated secret keeps) is not live
// either: the frozen source no longer names the secret the policy decides about, so the session
// asks again. A name whose requesters tag no longer lists this requester is not live either, and
// ends the grant for good (errRequesterNotListed): that list is who may ask for the secret at
// all, so a grant made before it dropped them must not go on releasing while they are off it. A
// name whose requesters tag names a group not read in the last two hours is not live while it is
// so, and the grant is not ended (policy.ReasonRequesterGroupUnread): the list may still name the
// person, and a fresh read says so.
// A grant the model judge decided — record.JudgeLogin reads a judge actor off its decidedBy — is
// held to the rules instead of to that login, and to the admin tag over both: the secret must not
// be an administrator's now, and the rules in force must still give the name a judge of at least
// the seconds the grant was given (judgeStillDecides, which set and lifetime carry here).
// No judge login goes through record.MayDecide, which refuses every one of them.
func stillAllowed(set *policy.Set, rules proxyrules.Rules, name, frozenSource, frozenDecision, decidedBy string, lifetime *int, requester policy.Requester) error {
	d, err := set.Evaluate(name, requester)
	if errors.Is(err, policy.ErrUnknownSecret) {
		return noLongerServed(name)
	}
	if err != nil {
		return err
	}
	if set.Secrets[name].Source != frozenSource {
		return sourceMoved(name)
	}
	_, judged := record.JudgeLogin(decidedBy)
	switch {
	case d.Outcome == policy.Deny && d.Reason == policy.ReasonRequesterNotListed:
		return requesterNotListed(name)
	case d.Outcome == policy.Deny && d.Reason == policy.ReasonRequesterGroupUnread:
		// Paused, not ended: the list may still name this person, which a fresh read will say.
		return fmt.Errorf("%w: %s's requesters name a group whose members were not read in the last two hours", ErrGrantNotLive, name)
	case projectDeny(d):
		return projectNotNamed(name)
	case d.Outcome == policy.Deny:
		return fmt.Errorf("%w: the current policy no longer allows %s", ErrGrantNotLive, name)
	case d.Outcome != policy.Approval:
		return nil
	case frozenDecision == policy.Automatic && !slices.Contains(requester.Withheld, name):
		return fmt.Errorf("%w: the current policy requires approval for %s", ErrGrantNotLive, name)
	case judged:
		return judgeStillDecides(set, rules, name, lifetime)
	case !record.MayDecide(record.KindAgentSecret, d.Approver, "", decidedBy, set.Groups()):
		// A group-approved record whose group does not admit its decider now is the chain
		// check's to refuse and log (record.ChainVerifier.Verify, noteLapsedApprover), which runs
		// before this; here the policy's approver is not the one the grant was decided by.
		return fmt.Errorf("%w: %s now needs its owner's approval, which this grant does not have", ErrGrantNotLive, name)
	}
	return nil
}

// judgeStillDecides is stillAllowed's arm for a grant the model judge decided: the secret must
// not be one a person approves every release of under the live policy (an administrator's, or an
// Anthropic federation secret: policy.Secret.PersonApprovesEveryRelease), and the rules in force
// must still give name a
// judge of at least lifetime, the seconds the grant was given. The judge's authority is that
// entry and not a person's, so this arm reads the entry where the person's arm reads a login: a
// secret handed to another person ends a grant that person must approve and did not, and leaves
// a judged one alone, no person's approval being what it rested on, while a rules file that
// stops letting the judge decide this name, or that lowers it below what this grant holds, ends
// it. An entry that changed at all already ends the grant through the frozen
// request_secrets.airlock_digest, so this states the same rule where a reader of the release
// checks it, over the one thing the judge's approval rested on.
//
// The admin tag is read first and off the live policy set, because it outranks whatever seconds
// a rules file gives the name: the tag's promise is that a person approves every release of the
// credential (policy.TagAdmin), and a release is exactly what this is deciding, so an owner who
// tags a secret an administrator's after the judge decided it stops the releases a model
// approved rather than only the next decision (judge's admin check, currentPolicyAdmits). The
// promise is the secret's and not the rules file's to make, so no judge entry answers it, and
// the refusal ends the grant for good (errJudgedAdminSecret): the session asks again, and the
// judge then refuses it at decision time. A person's own grant of an admin secret is untouched,
// this arm being a judged grant's alone — a person approved it, which is all the tag asks.
//
// A grant with no lifetime — one that lasts until its session ends — is not live under a judge
// decider: record.MayJudge refuses a record that names no lifetime, since an unbounded grant is
// exactly what the judge's seconds are there to bound, so the judge made no such grant and no
// such grant is its.
func judgeStillDecides(set *policy.Set, rules proxyrules.Rules, name string, lifetime *int) error {
	if set.Secrets[name].PersonApprovesEveryRelease() {
		return judgedAdminSecret(name)
	}
	bound, ok := rules.JudgeSeconds(name)
	switch {
	case lifetime == nil:
		return fmt.Errorf("%w: %s was granted until its session ends, which the judge never decides", ErrGrantNotLive, name)
	case !ok:
		return fmt.Errorf("%w: the rules no longer let the judge decide %s", ErrGrantNotLive, name)
	case !record.JudgeAllowsLifetime(bound, *lifetime):
		return fmt.Errorf("%w: the rules now let the judge decide at most %d seconds of %s, short of this grant's %d", ErrGrantNotLive, bound, name, *lifetime)
	}
	return nil
}

// errSourceMoved is stillAllowed's refusal of a name served from another source than its grant
// froze: a type changed or removed, a secret replaced on AWS, the broker moved between stores.
// Values ends such a grant for good (endGrant).
var errSourceMoved = errors.New("source moved")

// errRequesterNotListed is stillAllowed's refusal of a granted name the secret's requesters tag
// no longer lists this requester on: the list was narrowed after the grant was made. Values and
// reuseLiveGrant end such a grant for good (endGrant), so putting the person back on the list
// never brings back a grant they held while they were off it; they ask again instead.
var errRequesterNotListed = errors.New("requester not listed")

// errJudgedAdminSecret is judgeStillDecides's refusal of a judged grant of a secret a person now
// approves every release of under the live policy (policy.Secret.PersonApprovesEveryRelease): one
// tagged as an administrator's (policy.TagAdmin) after the model judge decided the request, or one
// retagged to the anthropic-federation type. Values and reuseLiveGrant end such a grant for good
// (endGrant), so taking the tag off again never brings back a grant no person approved; the
// session asks again, as a new request, which the judge refuses while the tag stands.
var errJudgedAdminSecret = errors.New("judged administrator's secret")

// JudgePolicy answers what the live policy says about name for the enrollment that asked,
// whether that name is withheld from its session, and whether a person approves every release of
// the secret (policy.Secret.PersonApprovesEveryRelease: an administrator's, or an Anthropic
// federation secret):
// the one question the broker's model judge asks the machine before it decides a pending request
// (judge.Requests, whose other two methods are the writes a verdict makes — ApplyJudgeDecision
// and LogJudged, in machine_state.go).
//
// It re-reads everything now rather than taking what the request froze. The policy comes from
// m.Policy.Get(), the snapshot a request made minutes ago may no longer have been decided under,
// and the requester from the database through the same m.requester Create decides with, so the
// person the session acts for, the service its account still proves and the names its operator
// has taken back from it are read exactly as a decision reads them, rather than a second time in
// words of this method's own.
//
// The withheld flag is answered beside the decision because Evaluate folds a withheld name into
// its outcome — a withheld agent-tier name comes back as an approval rather than as automatic —
// so the two are not one answer: the judge decides no name its session's operator withheld,
// whatever the policy would otherwise give it.
//
// The third flag is answered beside it for the same sort of reason and off the same snapshot:
// Evaluate folds an admin tag, and a federation type, into nothing at all — such a secret is human
// tier and comes back as the ordinary approval its owner makes — so the outcome alone cannot tell
// a credential a person approves every release of from any other the owner approves. The judge
// decides none of them, whatever seconds a rules file gives it for the name, and the one snapshot
// both answers come off is what keeps the tag and the outcome from being read a policy apart.
//
// An enrollment that is no longer live (revoked, or its lease lapsed) is an error carrying
// pgx.ErrNoRows, never a decision: the judge's check fails and the request waits for its person,
// where a silent approval would grant a credential to a box that has gone.
func (m *Machine) JudgePolicy(ctx context.Context, enrollmentID, name string) (policy.Decision, bool, bool, error) {
	enr, err := m.enrollment(ctx, enrollmentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return policy.Decision{}, false, false, fmt.Errorf("enrollment %s is no longer live: %w", enrollmentID, err)
		}
		return policy.Decision{}, false, false, err
	}
	set := m.Policy.Get()
	// The session's own rows first, so nothing stands between Dispatch's answers and the
	// evaluation that decides on them (projectFacts, atDecision).
	own, err := m.requester(ctx, m.Store.Pool, enr, projectFacts{})
	if err != nil {
		return policy.Decision{}, false, false, err
	}
	// What Dispatch holds of a tagged name is read under the judge's bound on Dispatch, whatever
	// ctx the caller passed (JudgeDispatch): this is the judge's first read of Dispatch, and it
	// shares the judge's Dispatch phase when the judge asks.
	reads, cancel := JudgeDispatch(ctx)
	facts, err := m.projectFacts(reads, enr, []string{name}, set)
	cancel()
	if err != nil {
		return policy.Decision{}, false, false, err
	}
	requester := facts.requester(own)
	decision, err := set.Evaluate(name, requester)
	if err != nil {
		return policy.Decision{}, false, false, err
	}
	return decision, slices.Contains(requester.Withheld, name), set.Secrets[name].PersonApprovesEveryRelease(), nil
}

// JudgeRequestState answers the state a decision on recordID would meet now: the second
// question the broker's model judge asks the machine (judge.Requests), read immediately before
// it spends a model call on the request.
//
// It answers what applyDecisionAttempt's own state read answers, in the same words: "pending"
// for a request still waiting on its person, "expired" for one whose pending_expires_at has
// passed though the sweep has not written that transition yet — the decision transaction
// answers such a request ErrExpired, so a judge that asked a model about it would have paid for
// an approval the machine then refused — and the row's own state for one already granted,
// denied, cancelled or expired. The request is the one the record names, as every read of this
// package is, and the read takes no lock: it is a gate on work the judge is about to start and
// never the check a decision is made under, which applyDecisionAttempt makes again under its
// own `for update`.
//
// A record with no request row is pgx.ErrNoRows, which the judge records as a state it could
// not read and asks nothing on.
func (m *Machine) JudgeRequestState(ctx context.Context, recordID string) (string, error) {
	var state string
	var expired bool
	if err := m.Store.Pool.QueryRow(ctx, `select state, pending_expires_at <= now() from requests where record_id=$1`, recordID).
		Scan(&state, &expired); err != nil {
		return "", err
	}
	if state == "pending" && expired {
		return "expired", nil
	}
	return state, nil
}

// Get reads one request by id. Its Approver is the approver column of its credential-request
// record, the column PendingForApprover lists the record by, so the request names exactly the
// person whose list shows it; a request with no record has none. JudgeDeciding is whether the
// judge holds it now: pending, with its judge_until ahead.
func (m *Machine) Get(ctx context.Context, id string) (Request, error) {
	var r Request
	var enrollmentID string
	err := m.Store.Pool.QueryRow(ctx, `select r.id, r.state, r.enrollment_id, r.lifetime_seconds, r.decided_at, r.decided_by, r.decision_detail, r.record_id, cr.approver,
		(select g.id::text from grants g where g.request_id=r.id and g.revoked_at is null limit 1), r.state='pending' and coalesce(r.judge_until > now(), false)
		from requests r left join credential_requests cr on cr.id = r.record_id where r.id=$1`, id).
		Scan(&r.ID, &r.State, &enrollmentID, &r.LifetimeSeconds, &r.DecidedAt, &r.DecidedBy, &r.Detail, &r.RecordID, &r.Approver, &r.GrantID, &r.JudgeDeciding)
	if err != nil {
		return Request{}, err
	}
	rows, err := m.Store.Pool.Query(ctx, `select name, decision from request_secrets where request_id=$1 order by name`, id)
	if err != nil {
		return Request{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var d SecretDecision
		if err := rows.Scan(&d.Name, &d.Decision); err != nil {
			return Request{}, err
		}
		r.Secrets = append(r.Secrets, d)
	}
	return r, rows.Err()
}

// OwnerAndState reads whose request requestID is, its state, and whether the model judge holds it
// (pending with judge_until ahead), in one statement. The owner is the owner of the enrollment
// the request hangs off now (Machine.owner). A caller may read and cancel the request when its own
// enrollment has the same owner (EnrollmentOwner). So a request is any enrollment's of the
// person, the Legion claim or the machine that made it, and an enrollment with no owner's alone.
// A person's pod shares its person's owner only while BROKER_POD_OPERATORS maps its account to
// that person. It reads the request's enrollment as it stands, which an adoption rewrites, so a
// request that moved with its session belongs to whoever it moved to. GET /v1/requests/{id}
// checks it before it answers, and its held read reads it again at each step while the request is
// pending. A request with no row is pgx.ErrNoRows.
func (m *Machine) OwnerAndState(ctx context.Context, requestID string) (owner, state string, judging bool, err error) {
	owner, err = m.scanOwner(m.Store.Pool.QueryRow(ctx, `select `+ownerColumns("e")+`, r.state, coalesce(r.state = 'pending' and r.judge_until > now(), false)
		from requests r join enrollments e on e.id = r.enrollment_id where r.id=$1`, requestID), &state, &judging)
	return owner, state, judging, err
}

// EnrollmentOwner is OwnerAndState's owner for an enrollment (Machine.owner). A caller's proof
// names a live enrollment, so pgx.ErrNoRows means it ended under the call.
func (m *Machine) EnrollmentOwner(ctx context.Context, enrollmentID string) (string, error) {
	return m.scanOwner(m.Store.Pool.QueryRow(ctx, `select `+ownerColumns("e")+` from enrollments e where e.id=$1`, enrollmentID))
}

// ownerColumns is the one SQL text of the columns of the enrollment aliased alias that
// Machine.owner reads: the owner it records, its id, kind, operator and subject. OwnerAndState,
// EnrollmentOwner and Cancel select these columns first, and scanOwner reads them.
func ownerColumns(alias string) string {
	return alias + `.owner, ` + alias + `.id::text, ` + alias + `.kind, ` + alias + `.operator, ` + alias + `.subject`
}

// scanOwner scans row, whose first columns are ownerColumns, and answers the owner that
// Machine.owner computes from them. It scans the columns after them into rest.
func (m *Machine) scanOwner(row pgx.Row, rest ...any) (string, error) {
	var recorded *string
	var enr enrollmentRow
	if err := row.Scan(append([]any{&recorded, &enr.ID, &enr.Kind, &enr.Operator, &enr.Subject}, rest...)...); err != nil {
		return "", err
	}
	return m.owner(recorded, enr), nil
}

// owner is whose requests enrollment enr makes, which OwnerAndState, EnrollmentOwner and Cancel
// compare. It is the owner that the enrollment records (recorded: enrollments.owner, enroll's
// ownerKey), or 'enrollment:<id>' for an enrollment that records none. No other enrollment's
// owner can spell that text. A person's pod records its person's owner, but it has that owner
// only while BROKER_POD_OPERATORS maps its account to that person (unmappedPersonPod, the test
// that requester reads too). After the setting drops the account, or maps it to another person,
// the pod's owner is 'enrollment:<id>'. The pod then reads and cancels its own requests alone,
// and its person reads none of them. The setting is in Go, so the owner is computed here and not
// in SQL.
func (m *Machine) owner(recorded *string, enr enrollmentRow) string {
	if recorded == nil || m.unmappedPersonPod(enr) {
		return "enrollment:" + enr.ID
	}
	return *recorded
}

// SessionID answers a request row's own optional session_id — an override supplied in the
// request body at Create time, distinct from the enrollment's own session_id — for
// wake.Envoy's notification target. main.go's waker tries this first and discards its error, so
// a null session_id or a request id that no longer exists both answer ("", nil); only a genuine
// Postgres failure is a non-nil error.
func (m *Machine) SessionID(ctx context.Context, id string) (string, error) {
	var sessionID *string
	err := m.Store.Pool.QueryRow(ctx, `select session_id from requests where id=$1`, id).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if sessionID == nil {
		return "", nil
	}
	return *sessionID, nil
}

// Grant is a live grant's public summary, for GET /v1/enrollments/self. It follows the same query
// pattern as OwnerAndState and enrollment above.
type Grant struct {
	// The grant's id, which the values and revoke routes take.
	ID string `json:"grant_id"`
	// The request it answered.
	RequestID string `json:"request_id"`
	// The login that approved it; null for a grant the policy gave automatically.
	Approver *string `json:"approver"`
	// When it expires; null for a grant that lasts until its session ends.
	ExpiresAt *time.Time `json:"expires_at"`
}

// LiveGrants lists this enrollment's own live (not revoked, not expired) grants, soonest to
// expire first and those that last until the session ends last.
// The lookup is the grants row's own enrollment_id, which an adoption rewrites, so a grant that
// followed its session to another box is listed under the enrollment the session proves now and
// under no other.
func (m *Machine) LiveGrants(ctx context.Context, enrollmentID string) ([]Grant, error) {
	rows, err := m.Store.Pool.Query(ctx, `select id, request_id, approver, expires_at from grants g
		where enrollment_id=$1 and revoked_at is null and `+grantUnexpired("g")+` order by expires_at nulls last, created_at`, enrollmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ID, &g.RequestID, &g.Approver, &g.ExpiresAt); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// grantUnexpired is the SQL predicate that grant alias's expiry has not passed: true for a grant
// with no expiry, which lasts until its session ends. Every read and state transition that asks
// whether a grant is live uses it beside revoked_at is null.
func grantUnexpired(alias string) string {
	return "(" + alias + ".expires_at is null or " + alias + ".expires_at > now())"
}

// insertGrant writes a grant that lasts lifetimeSeconds from now, or, when nil, until its session
// ends: a null expires_at, which endEnrollment's revoke alone ends.
func insertGrant(ctx context.Context, tx pgx.Tx, requestID, enrollmentID, approver string, lifetimeSeconds *int) (string, error) {
	id := uuid.NewString()
	_, err := tx.Exec(ctx, `insert into grants (id, request_id, enrollment_id, approver, expires_at)
		values ($1,$2,$3,$4, now() + make_interval(secs => $5))`,
		id, requestID, enrollmentID, nullable(approver), lifetimeSeconds)
	return id, err
}

type pendingRequestRow struct {
	ID       string
	Lifetime *int
	Names    []string
	// Legacy is a request written before migration 0013 (writtenBeforeMigration13), which asked
	// for no lifetime and recorded the release's fixed one.
	Legacy bool
	// RecordBody and RecordCreatedAt are the request's credential-request record: its canonical
	// body and when it was written, which is what signedForAnotherBroker verifies it against.
	// They are pointers because the left join reads NULL in both for a request with no record,
	// which pgx scans into no string and no time.Time.
	RecordBody      *string
	RecordCreatedAt *time.Time
}

// pendingRequests returns every pending request of the enrollment for any name sorted names
// shares whose deadline has not passed (requestBeforeDeadline), oldest first, with its names, its
// lifetime and its credential-request record. Create holds every requested name's advisory lock
// before it asks (lockPendingNames), so no other Create of the enrollment can pass the lookup and
// open another record for one of those names, and one read sees every overlapping request there
// is. endBesideReusedGrant first reads straight on the pool, under no lock, only to tell whether
// any request is signed for another broker address, and ends what a second read under those locks
// finds.
// A request past its deadline, though the sweep has not ended it yet, is neither kept, ended
// nor in the way: every decision of it answers ErrExpired, so a session coalesced onto it would
// wait on an answer that can never come, its approver seeing only a request nobody can approve,
// and one refused over it (ErrRequestPending) could ask again only once the sweep had run. The new
// request opens a record of its own instead, listed pending beside the expired one, and the sweep
// expires the old one, whichever broker address its record is signed for.
// It reads the requests row's own enrollment_id, so a card that moved with its session is found
// under the enrollment holding it now: the moved session coalesces onto the card a person is
// already holding rather than opening a second one, and the box it moved off, which holds the
// card no longer, is refused nothing.
// It reads each request's credential-request record beside it, so Create can tell each one
// signed for another broker address (signedForAnotherBroker) and end it, and keep every other one
// (endSignedForAnotherBroker). The record is a left join, so a request with no record is still
// found. The order is the request's created_at and then its id, a total order, so two
// transactions that end the same requests take their row locks in one order.
// It reads every row and closes them before it returns: a caller that ends requests does so in
// the same transaction, and a pgx transaction runs no statement while rows of its connection are
// open.
func pendingRequests(ctx context.Context, q querier, enrollmentID string, sorted []string) ([]pendingRequestRow, error) {
	rows, err := q.Query(ctx, `select req.id, req.lifetime_seconds, array_agg(s.name order by s.name), `+writtenBeforeMigration13("req")+`,
			cr.body, cr.created_at
		from requests req join request_secrets s on s.request_id=req.id
		left join credential_requests cr on cr.id = req.record_id
		where req.enrollment_id=$1 and req.state='pending' and `+requestBeforeDeadline+` and exists (
			select 1 from request_secrets matching where matching.request_id=req.id and matching.name = any($2)
		) group by req.id, req.lifetime_seconds, req.created_at, cr.id, cr.body, cr.created_at order by req.created_at, req.id`, enrollmentID, sorted)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (pendingRequestRow, error) {
		var p pendingRequestRow
		err := row.Scan(&p.ID, &p.Lifetime, &p.Names, &p.Legacy, &p.RecordBody, &p.RecordCreatedAt)
		return p, err
	})
}

// joins answers whether a request asking for lifetime joins this pending request: the same
// lifetime, or, for a request asking for none, one written before migration 0013, which asked for
// none and recorded the release's fixed lifetime.
func (p pendingRequestRow) joins(lifetime *int) bool {
	return sameLifetime(p.Lifetime, lifetime) || (lifetime == nil && p.Legacy)
}

func sameLifetime(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// signedForAnotherBroker reports whether pending request p's record is signed for another broker
// address: its request object does not verify for this broker, by the check PendingForApprover
// lists a record by (requestedIdentifiers), because its aud is not Machine.Audience
// (record.ErrAudienceMismatch). Such a request can be decided by no person and no judge here: no
// approver's list shows it and the judge was never handed it. A request with no record, a body
// that does not parse and every other refusal (a signature, a time) answer false, so Create keeps
// such a request and applies the one-pending-request rule to it: the broker's move is the one
// reason it ends one.
func (m *Machine) signedForAnotherBroker(p pendingRequestRow) bool {
	if p.RecordBody == nil || p.RecordCreatedAt == nil {
		return false
	}
	_, _, err := m.requestedIdentifiers(*p.RecordBody, *p.RecordCreatedAt)
	return errors.Is(err, record.ErrAudienceMismatch)
}

// querier is what a read takes to run inside a transaction or straight on the pool.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// liveMatch is one row matchingRequests answers: a request whose names are exactly the ones asked
// for, and the live grant the row named beside it.
type liveMatch struct {
	requestID, grantID string
}

// matchingRequests runs query (whose rows are a request id, that request's own live grant's id
// and that request's secret names, with $1 bound to enrollmentID and $2 to lifetime, the lifetime
// the request must have asked for) and answers every request whose names are exactly sorted, each
// with the grant its row named, in the query's own order, or none. Names are compared as sets of
// whole strings, never as one joined string, so a name containing a separator cannot pass for
// two names. The grant's id is read beside the request's rather than looked up again because
// reuseLiveGrant ends exactly the grant this row matched when the policy no longer allows it.
// It reads every row and closes them before it returns, so no connection is held open while the
// caller checks each match, which reads and writes on connections of its own.
// Which column $1 is compared against is the query's own; every query here names the requests
// row's own enrollment_id, the column an adoption rewrites.
func matchingRequests(ctx context.Context, q querier, query, enrollmentID string, lifetime *int, sorted []string) ([]liveMatch, error) {
	rows, err := q.Query(ctx, query, enrollmentID, lifetime)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var matches []liveMatch
	for rows.Next() {
		var match liveMatch
		var names []string
		if err := rows.Scan(&match.requestID, &match.grantID, &names); err != nil {
			return nil, err
		}
		sort.Strings(names)
		if slices.Equal(names, sorted) {
			matches = append(matches, match)
		}
	}
	return matches, rows.Err()
}

// writtenBeforeMigration13 is the SQL predicate that request alias was written before migration
// 0013 (grants until the session ends) was applied, recording that release's fixed 43200-second
// lifetime although it asked for none. A request without a lifetime takes such a request's live
// grant, keeping its expiry, and joins it while it is pending, so a session running across the
// deploy neither asks its approver again nor is refused. The request's own time decides, since it
// is the row that recorded the old default: a request pending at the deploy and approved after it
// is covered too. No request written after the deploy matches, so a later --ttl 12h request is
// never taken for one, and the bridge ends on its own, with nothing to remove: such a request
// waits at most 12 hours and its grant lasts 12 more, so none matches a day after the deploy.
func writtenBeforeMigration13(alias string) string {
	return "(" + alias + ".lifetime_seconds = 43200 and " + alias +
		".created_at < (select applied_at from broker_schema_migrations where version = 13))"
}

// reuseLiveGrant answers Create's own "request (or reuse the live grant)" contract: an exact
// name-set match (never a subset or superset — the same matching rule coalescing uses for pending
// requests), asking for the same lifetime (or, asking for none, a request written before
// migration 0013: writtenBeforeMigration13), against a still-live grant (not revoked, not expired)
// under this enrollment is returned as-is, with no new request row and no new record, as long as
// the policy it was decided under is still current, no granted name is a service's
// (anyServiceOwned: the policy's version does not cover which service BROKER_SERVICES' accounts
// prove), no granted name carries a requesters tag (anyListed: an entry naming a service
// matches by those same accounts, and one naming a group by the group's members), no granted name
// is a group's (anyGroupOwned: the version does not cover the group's members), no granted name
// is tagged with a project (anyProjectTagged: whether it is private is Dispatch's) and the grant
// is no person's pod's (personPodGrant: the policy's version does not cover which person
// BROKER_POD_OPERATORS maps the pod's account to), or the current policy still allows it to the
// session (stillAllowed, the check Values makes, which holds a judged grant to the judge the rules
// still give its name, over the lifetime this reuse matched it on, the grant's own, and refuses
// one whose secret the policy now tags as an administrator's), and its whole approval chain still
// verifies (verifyChainForRelease, the same check Values makes, read first). A caller that already
// holds a live grant for these exact names never re-asks a human who already approved it, a policy
// tightened since then is never bypassed by reuse, and neither is a chain that no longer verifies.
// Every exact match is a candidate, newest grant first, and each gets those checks in turn. A
// candidate whose chain fails, or whose group approval has lapsed, is passed over for the next,
// and logged (GrantChainInvalidMessage, GrantApproverLapsedMessage, at=reuse). A grant a
// requesters group not read in the last two hours pauses (policy.ReasonRequesterGroupUnread)
// answers no reusable grant and is not ended, as Values leaves it. A session's next enrollment
// adopts its earlier enrollment's live grants (enroll.go, adopt), so a session that moved here
// from a broker of another BROKER_PUBLIC_URL sharing this
// database can hold that broker's grant, whose chain fails here ("aud does not match"), and,
// after its first grant here, a newer one of this broker for the same names. Without the order,
// Postgres may answer the old grant first on any call, which would ask a person or the judge
// again on each until the old grant expired. Newest first,
// the grant the session was given last is the one handed back, and an older one is checked, and
// its failed chain logged, only when every newer one was passed over. Every other outcome stops
// at that candidate, as with one: a policy end (below) answers no reusable grant, a grant no
// longer live answers none, and any other error is returned. With no candidate left there is
// none, and Create asks afresh.
// A grant whose secret no longer lists this requester, and a judged grant of a secret a person now
// approves every release of (an administrator's, or an Anthropic federation secret), are not
// merely passed over: each is ended for good here, as
// Values ends it (errRequesterNotListed, errJudgedAdminSecret), and Create goes on to ask for
// the names afresh — the denied request that tells the session why, where the list dropped it,
// and the card the tag's own person answers, where the tag ended it.
// An automatic grant a person's pod got as that person is not ended once BROKER_POD_OPERATORS no
// longer maps the pod's account to that person, but its re-check refuses it (ErrGrantNotLive),
// which answers no reusable grant, so Create asks for the names afresh. The grant is reusable
// again when the entry comes back. The exception is a grant whose secret's requesters tag names
// the person: the secret no longer lists the requester, so the grant is ended for good, as the
// paragraph above says (errRequesterNotListed).
// No live grant holds a withheld name nobody approved: the withhold ended each one (withhold).
// The lookup reads the requests row's own enrollment_id, which an adoption rewrites, so a session
// that enrolled on another box gets back the grant that moved with it instead of asking a person
// a second time for names it already holds
// (TestTheMovedSessionAsksAgainAndGetsTheSameGrantBack).
func (m *Machine) reuseLiveGrant(ctx context.Context, enr enrollmentRow, sorted []string, lifetime *int, set *policy.Set, facts projectFacts) (Request, bool, error) {
	matches, err := matchingRequests(ctx, m.Store.Pool, `select r.id, g.id::text, array_agg(s.name) from requests r
		join request_secrets s on s.request_id=r.id
		join grants g on g.request_id=r.id
		where r.enrollment_id=$1 and (
			r.lifetime_seconds is not distinct from $2 or ($2::int is null and `+writtenBeforeMigration13("r")+`)
		) and r.state='granted' and g.revoked_at is null and `+grantUnexpired("g")+`
		group by r.id, g.id order by g.created_at desc`, enr.ID, lifetime, sorted)
	if err != nil {
		return Request{}, false, err
	}
candidates:
	for _, match := range matches {
		var rulesVersion, decidedBy string
		if err := m.Store.Pool.QueryRow(ctx, `select rules_version, coalesce(decided_by, '') from requests where id=$1`, match.requestID).Scan(&rulesVersion, &decidedBy); err != nil {
			return Request{}, false, err
		}
		granted, err := requestedSecrets(ctx, m.Store.Pool, match.requestID)
		if err != nil {
			return Request{}, false, err
		}
		// The chain first, as a release checks it: one that fails, or a group approval that has
		// lapsed, is passed over for the next candidate, its lines written as a release writes
		// them.
		if recordID, err := m.verifyChainForRelease(ctx, match.grantID, enr.ID, "reuse"); err != nil {
			noteLapsedApprover(match.grantID, recordID, "reuse", granted, err)
			if errors.Is(err, ErrGrantChainInvalid) || errors.Is(err, ErrGrantNotLive) {
				continue candidates
			}
			return Request{}, false, err
		}
		if rulesVersion != set.Version || anyServiceOwned(set, granted) || anyListed(set, granted) || anyGroupOwned(set, granted) || anyProjectTagged(set, granted) || personPodGrant(enr) {
			requester, err := m.requester(ctx, m.Store.Pool, enr, facts)
			if err != nil {
				return Request{}, false, err
			}
			for _, g := range granted {
				err := stillAllowed(set, m.Airlock, g.name, g.source, g.decision, decidedBy, lifetime, requester)
				switch {
				case errors.Is(err, errRequesterNotListed) || errors.Is(err, errJudgedAdminSecret) || errors.Is(err, errProjectNotNamed):
					// The list dropped this requester while the grant was live, the secret
					// became an administrator's, which a person approves every release of, or
					// the credential no longer names the private project the secret is tagged
					// with: end the grant here as Values does, rather than leave it live and
					// unrevoked, and answer no reusable grant so Create goes on to ask for the
					// names afresh.
					if ended := m.endGrant(ctx, match.grantID, err); !errors.Is(ended, ErrGrantNotLive) {
						return Request{}, false, ended
					}
					return Request{}, false, nil
				case errors.Is(err, ErrGrantNotLive):
					return Request{}, false, nil
				case err != nil:
					return Request{}, false, err
				}
			}
		}
		r, err := m.Get(ctx, match.requestID)
		if err != nil {
			return Request{}, false, err
		}
		return r, true, nil
	}
	return Request{}, false, nil
}

// RecordKind reads a credential-request record's own kind ("agent_secret" or
// "launcher_credential") — the one column api.decideRecord needs before it knows which service's
// ApplyDecision a record id belongs to. pgx.ErrNoRows means no such record.
func (m *Machine) RecordKind(ctx context.Context, recordID string) (string, error) {
	var kind string
	err := m.Store.Pool.QueryRow(ctx, `select kind from credential_requests where id=$1`, recordID).Scan(&kind)
	return kind, err
}

// JudgeHolds reports whether the model judge holds the request of a credential-request record now:
// the request is pending and its judge_until lies ahead. It reads that one fact and not the
// record, so api.decideRecord can say JUDGE_DECIDES for a record whose signed request verifies for
// no audience of this broker's, which ReadRecord refuses. A record with no request (a machine
// login) is held by nobody.
func (m *Machine) JudgeHolds(ctx context.Context, recordID string) (bool, error) {
	var held bool
	err := m.Store.Pool.QueryRow(ctx, `select coalesce(bool_or(state = 'pending' and judge_until > now()), false)
		from requests where record_id=$1`, recordID).Scan(&held)
	return held, err
}

// Session is the two possibly-differing session ids a credential-request record can name, read
// independently (LEGION-587): Enrollment is the id the requesting session's own enrollment stated
// when it enrolled (enrollments.session_id — agentbox's --session-id, or the Legion daemon's
// worker claim session), Request is the id the request itself stated as an unsigned override in
// its body at Create time (requests.session_id). Either is empty when that id was never set; a
// machine login (kind launcher_credential, which has no request row and no requesting enrollment)
// always answers both empty. An id outlives its enrollment's revocation: enrollments.session_id is
// never cleared, only revoked_at is set, so the join below still finds it.
type Session struct {
	Request    string
	Enrollment string
}

// PendingSummary is one credential-request record for GET /v1/pending: its id, kind, the plain
// identifiers its request object names (secret names for agent_secret, the single host for
// launcher_credential), when it was requested, the session it names, its state and its deadline.
type PendingSummary struct {
	RecordID    string
	Kind        string
	Identifiers []string
	RequestedAt time.Time
	Session     Session
	// State is "pending" for a record a decision may still settle, and "expired" for one whose
	// deadline passed before anybody decided it, which every decision refuses (recordStateSQL).
	State string
	// ExpiresAt is the record's deadline (recordDeadlineSQL): a secret request's
	// requests.pending_expires_at, the column its decision reads, and a machine login's
	// credential_requests.expires_at.
	ExpiresAt time.Time
}

// PendingStates is which records PendingForApprover lists. Its zero value asks for no state and
// lists what GET /v1/pending listed before a record had a state: every record still pending by
// what is stored, its deadline passed or not, until the expiry sweep ends it. The approval page
// lists with it, and so does a Dispatch build that sends no state. A set field asks for the
// records of that state alone, as a two-field struct so that no caller can ask for a state that
// does not exist.
type PendingStates struct {
	// Pending lists every record a decision may still settle: nobody has decided it, and its
	// deadline has not passed.
	Pending bool
	// Expired lists every record whose deadline passed before anybody decided it and that nobody
	// has dismissed (Dismiss), before the expiry sweep ends it and after.
	Expired bool
}

// requestBeforeDeadline is the SQL predicate that req, a requests row, has not reached its
// deadline: its pending_expires_at is after now(), the moment the decision compares it with
// (applyDecisionAttempt's `pending_expires_at <= now()`). A request with no pending_expires_at
// has no deadline to pass. Create writes one on every request it leaves pending (insertRequest),
// and has since the first broker, so only a request decided at once has none, and such a request
// has no record; the rule says what a row with none would mean rather than leave it to whichever
// reader meets it. The sweep never expires such a row (store.RequestsPastDeadline) and no decision
// refuses it as expired, so it stays pending, as it always read. It is a constant over one alias,
// rather than a function of the alias, so the constant statements below can be built from it.
const requestBeforeDeadline = `(req.pending_expires_at is null or req.pending_expires_at > now())`

// recordStateSQL is the one SQL rule for whether a credential-request record is pending, held for
// the model judge or expired, over cr, its credential_requests row, and req, its requests row left
// joined on requests.record_id (none for a machine login). It is 'pending' for a record a person's
// decision may still settle, 'held' for a secret request the model judge holds before its
// deadline, 'expired' for a record whose deadline passed before anybody decided it, and null for
// every other record: one decided, or a secret request cancelled before its deadline, which never
// reached it. Expired is exactly what a decision refuses as expired, with the database's now():
//
//   - A secret request by its request row: 'expired', which the sweep wrote; or 'pending' past its
//     pending_expires_at (requestBeforeDeadline), which applyDecisionAttempt answers ErrExpired
//     before the sweep gets to it, the judge's hold or none; or 'cancelled' at or after that
//     deadline (pending_expires_at <= decided_at), a cancel that came after the deadline had
//     passed, which the requesting session (Cancel) or its enrollment's end writes on any pending
//     request whatever its deadline. Before its deadline, one with judge_until ahead is 'held': it
//     is no person's yet, so no list shows it and Dismiss refuses it as not expired; once the hold
//     ends (handed over, or run out) it is 'pending'.
//   - A machine login by its terminal event: an 'expired' event, which the sweep wrote
//     (machine.Service.ExpirePending); or none, with expires_at passed, which machine's
//     ApplyDecision answers ErrLoginExpired. Nothing cancels a machine login.
//
// The terminal-event predicate is spelled literally, as pendingForApproverQuery's doc comment says
// why, so the subquery reads credential_request_decision. PendingForApprover lists by it, Dismiss
// refuses by it, and ReadRecord reads a record still pending past its deadline as expired by it.
const recordStateSQL = `case cr.kind
		when 'agent_secret' then case
			when req.state = 'pending' and ` + requestBeforeDeadline + ` and req.judge_until > now() then 'held'
			when req.state = 'pending' and ` + requestBeforeDeadline + ` then 'pending'
			when req.state = 'pending' then 'expired'
			when req.state = 'expired' then 'expired'
			when req.state = 'cancelled' and req.pending_expires_at <= req.decided_at then 'expired'
		end
		when 'launcher_credential' then case coalesce((select ev.event from credential_request_events ev
				where ev.record_id = cr.id and ev.event in ('approved','denied','expired','cancelled')), '')
			when 'expired' then 'expired'
			when '' then case when cr.expires_at <= now() then 'expired' else 'pending' end
		end
	end`

// recordDeadlineSQL is the deadline of the record cr whose requests row is req, as recordStateSQL
// reads it: a secret request's pending_expires_at, the column its decision compares, and a machine
// login's own expires_at. Create writes the two columns of a secret request from one value
// (createPending), so the record's own expires_at stands in for a request row with none.
const recordDeadlineSQL = `coalesce(req.pending_expires_at, cr.expires_at)`

// pendingListFrom is the select list, the joins and the approver filter of both of
// PendingForApprover's statements, which differ only in what they keep of the approver's records
// and in their order. The approver scan is credential_requests_approver
// (store/migrations/0011_credential_requests_approver.up.sql) in both: either one reads every
// record of the approver, as the one statement before records had a state already did, and
// keeps those of the states it lists. Each row's state is recordStateSQL's, read once in a
// lateral of its own, so a statement that filters on it, orders by it and answers it reads one
// value. The lateral's `offset 0` keeps the planner from pulling it up into the statement, which
// would copy recordStateSQL into the filter, the select list and the sort key and run a machine
// login's terminal-event read once for each copy.
//
// The two left joins read each row's session (LEGION-587): req on requests.record_id, the same
// column requests_record (0008_lookup_indexes.up.sql) already indexes for ReadRecord, and enr on
// credential_requests' own enrollment_id, its primary-key lookup into enrollments. req is also
// the request row recordStateSQL and recordDeadlineSQL read.
//
// A session's move re-points its requests row and not the append-only record, so enr is the
// enrollment that asked and cur the one the request hangs off now. enr is read for its session id,
// and both for the projects their credentials name (credentialColumns, currentCredentialColumns,
// credentialsProjects): an adoption moves a session's rows only to another enrollment of the same
// session id (the carry pair enroll.Create matches on), so that id is the one the enrollment
// holding the request names too, but the projects the two credentials name can differ. Which
// records are pending comes from the requests rows, which move, so a moved card is listed once and
// the box it moved off lists it no more. A request the model judge holds (judge_until ahead) is
// no person's yet: recordStateSQL reads it as neither pending nor expired while its deadline has
// not passed, so neither statement lists it until its hold has ended (handed over, or run out),
// and past its deadline it reads expired, as every request does.
//
// An agent_secret record waiting on $2, record.AnyoneApprover, is read with the person its notify
// column names (responsiblePerson), and PendingForApprover chooses whose list holds it
// (listedInStead for a row whose responsible person is another). A launcher_credential record
// carries no notify, so it is listed for everyone. A record waiting on a group, group:<address>,
// is listed for each of the groups $3 names, the fresh groups the group reader lists the person in
// (Machine.Groups' GroupsOf), in the group form.
const pendingListFrom = `select cr.id, cr.kind, cr.body, cr.created_at, coalesce(req.session_id, ''), coalesce(enr.session_id, ''),
		s.state, ` + recordDeadlineSQL + `, coalesce(cr.notify, ''), ` + credentialColumns + `, ` + currentCredentialColumns + `
	from credential_requests cr
	left join requests req on req.record_id = cr.id
	left join enrollments enr on enr.id = cr.enrollment_id
	left join enrollments cur on cur.id = req.enrollment_id
	cross join lateral (select ` + recordStateSQL + ` as state offset 0) s
	where (cr.approver = $1 or cr.approver = $2 or cr.approver = any($3::text[])) and `

// credentialColumns are the enrollment's columns namedProjects reads, over a left join aliased
// enr, in the order a scan into enrollmentRow's Kind, Operator, Subject and Projects takes them. A
// record no enrollment asked answers an empty kind, which names no project.
const credentialColumns = `coalesce(enr.kind, ''), enr.operator, enr.subject, coalesce(enr.projects, '{}')`

// currentCredentialColumns are credentialColumns over a left join aliased cur: the enrollment a
// request hangs off now, where enr is the one that asked. A record with no request (a machine
// login) answers an empty kind, which names no project.
const currentCredentialColumns = `coalesce(cur.kind, ''), cur.operator, cur.subject, coalesce(cur.projects, '{}')`

// pendingForApproverQuery is PendingForApprover's statement for a caller that asks for no state
// (PendingStates' zero value): every record of the approver still pending by what is stored,
// newest first, which is exactly the list from before records had a state. An agent_secret record
// is that while its request row is 'pending' and the model judge does not hold it, and a machine
// login while it carries no terminal event, its deadline passed or not, so a record past its
// deadline stays on this list until the expiry sweep ends it, and leaves it then. Each row carries
// its state all the same, so a record past its deadline reads 'expired' here too. It and
// pendingOrExpiredForApproverQuery are pulled out so TestPendingForApproverAvoidsSequentialScans
// (same package) can PREPARE and EXPLAIN the exact statements the pool runs rather than copies
// that could drift from them.
//
// The launcher_credential branch's NOT EXISTS spells its terminal-event predicate literally
// (event in ('approved','denied','expired','cancelled')) rather than binding
// record.TerminalEventNames() as a parameter, and so does recordStateSQL's subquery, which both
// statements read, for the same reason machine.ExpirePending's ON CONFLICT target already does for
// the same index (credential_request_decision, store/migrations/0005_credential_requests.up.sql):
// Postgres can only recognize a partial index from a predicate it can prove implies the index's at
// plan time, which a bound parameter never does once the plan goes generic. The literal makes the
// index provably reachable in every plan shape; whether an unforced planner actually prefers it
// over hashing the far smaller matching set in one pass is then an ordinary cost comparison, which
// is why TestPendingForApproverAvoidsSequentialScans also sets enable_seqscan = off — that is what
// guarantees the index path, not the literal alone. A later change to the terminal event list must
// change these literals too, which TestPendingForApproverTerminalEventsLiteralMatchesTheList pins
// against record.TerminalEventNames().
//
// TestPendingForApproverAvoidsSequentialScans checks credential_requests,
// credential_request_events and credential_request_dismissals for a sequential scan; it pins
// nothing about how Postgres joins in req and enr (a hash join over requests and enrollments,
// measured locally at 5,000 enrollments — free to change with the planner's own cost estimates,
// and not this test's concern).
const pendingForApproverQuery = pendingListFrom + `(
		cr.kind='agent_secret' and cr.id in (select r.record_id from requests r where r.state='pending' and (r.judge_until is null or r.judge_until <= now()))
		or cr.kind='launcher_credential' and not exists (
			select 1 from credential_request_events ev where ev.record_id=cr.id and ev.event in ('approved','denied','expired','cancelled'))
	) order by cr.created_at desc`

// pendingOrExpiredForApproverQuery is PendingForApprover's statement for a caller that asks for
// states, $4 for the pending records and $5 for the expired ones: the records of the approver
// recordStateSQL reads 'pending' while $4 holds, and those it reads 'expired' that have no row in
// credential_request_dismissals (migration 0033) while $5 holds. The pending rows come first and
// the expired ones after them, each newest first, so a caller that cuts the list into pages from
// the front never pushes a record a person can still decide off its first page to show records
// nobody can.
// TestPendingForApproverAvoidsSequentialScans holds the dismissal's NOT EXISTS to the table's
// primary key, as it holds the rest of this statement to pendingForApproverQuery's indexes.
const pendingOrExpiredForApproverQuery = pendingListFrom + `(
		s.state = 'pending' and $4::boolean
		or s.state = 'expired' and $5::boolean and not exists (
			select 1 from credential_request_dismissals d where d.record_id = cr.id)
	) order by s.state = 'expired', cr.created_at desc`

// PendingForApprover lists the credential-request records — of either kind — that approver is
// asked to decide, and the states they are in: GET /v1/pending's exact contract, which Dispatch
// relays to each person's Inbox. Which records it lists is what states asks for: with no state,
// every record still pending by what is stored, newest first (pendingForApproverQuery); with
// states, the records a decision may still settle and the expired records nobody has dismissed,
// as states asks, the pending ones first (pendingOrExpiredForApproverQuery). Either way each row
// says whether it is pending or expired, and when its deadline is, by the one rule a decision
// refuses an expired record by (recordStateSQL), which ReadRecord and Dismiss read as well.
//
// An approver's records are every record naming approver; every record naming a group the group
// reader lists approver in now (Machine.Groups), so each member of a group owner is asked; every
// request for a shared human-tier secret, whose approver is record.AnyoneApprover, that names
// approver as the person responsible for the agent that made it, or names no such person
// (credential_requests.notify, Create's responsiblePerson), so it waits where everyone sees it, or
// names a person who may not see it (listedInStead); and every service's machine login, whose
// approver is record.AnyoneApprover too. A private project's request is on no list of a person
// outside the project (maySeeRecord). Who may decide is not this list's to say: anyone signed in
// decides a shared secret's request from its page whether or not it is listed for them
// (record.MayDecide), within the person rule. A service's login a broker from before that rule
// opened names a person
// instead and is listed for that person alone, though anyone who types its code may decide it
// (record.MayDecide). Whether an agent_secret record is still pending is read from its request
// row: every writer moves the request out of 'pending' together with the record's terminal event
// (store.EndPendingRequests, ApplyDecision), but a request an ended enrollment cancelled before
// endEnrollment wrote that event carries none, so the request row is the truth. A machine login
// has no request row and is pending while it carries no terminal decision event, matching
// credential_request_decision's own partial index — see pendingForApproverQuery's own comment for
// why that match requires a literal predicate, not record.TerminalEventNames() bound as a
// parameter. The approver scan itself is credential_requests_approver
// (store/migrations/0011_credential_requests_approver.up.sql): with 300,000 historical rows and
// no index, this was one sequential scan of the whole table on every 15 s poll tick of every open
// Dispatch tab (336 ms; LEGION-575's review).
//
// A record whose signed request does not verify for this broker — one signed for another
// BROKER_PUBLIC_URL, after the public URL changed while it waited or when a second broker shares
// this database — is left out of the list, and the list goes on. That record can be decided by no
// broker that cannot verify it, and returning its error instead answered 503 to the approval page
// and GET /v1/pending for every request of that approver until the record expired, up to the whole
// pending window. A record left out is logged once (leaveOut), with one exception: an expired
// record left out of a list that asks for states is not logged at all. Such a list reads that
// record until somebody dismisses it, which nobody does to a record no list shows, so a line about
// it would come back every leftOutRetention for as long as the broker runs and say nothing anybody
// can act on. A list with no state reads an expired record only until the sweep ends it, and its
// one line is the one it always wrote.
func (m *Machine) PendingForApprover(ctx context.Context, approver string, states PendingStates) ([]PendingSummary, error) {
	login := record.CanonicalLogin(approver)
	query, args := pendingForApproverQuery, []any{login, record.AnyoneApprover, m.groupApprovers(login, true)}
	byState := states != (PendingStates{})
	if byState {
		query, args = pendingOrExpiredForApproverQuery, append(args, states.Pending, states.Expired)
	}
	rows, err := m.Store.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingSummary
	// seen is what each agent-secret row's listing reads beside it: the credential projects and
	// reason the person rule (maySeeRecord) reads, and the person the record names as responsible
	// (notify). It is kept beside out and read only once the rows are closed, so no call to
	// Dispatch holds the pooled connection the read does.
	type seen struct {
		sessionProjects []string
		reason          string
		notify          string
	}
	var reads []seen
	for rows.Next() {
		var id, kind, canonical, requestSession, enrollmentSession, state, notify string
		var createdAt, expiresAt time.Time
		var asked, current enrollmentRow
		if err := rows.Scan(&id, &kind, &canonical, &createdAt, &requestSession, &enrollmentSession, &state, &expiresAt, &notify,
			&asked.Kind, &asked.Operator, &asked.Subject, &asked.Projects,
			&current.Kind, &current.Operator, &current.Subject, &current.Projects); err != nil {
			return nil, err
		}
		identifiers, reason, err := m.requestedIdentifiers(canonical, createdAt)
		if err != nil {
			if !byState || state != "expired" {
				m.leaveOut(id, kind, expiresAt, err)
			}
			continue
		}
		out = append(out, PendingSummary{
			RecordID: id, Kind: kind, Identifiers: identifiers, RequestedAt: createdAt,
			Session: Session{Request: requestSession, Enrollment: enrollmentSession},
			State:   state, ExpiresAt: expiresAt,
		})
		reads = append(reads, seen{sessionProjects: m.credentialsProjects(asked, current), reason: reason, notify: notify})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// Each agent-secret row is listed for the person responsible for it, and for everyone where it
	// names none (responsiblePerson), and in either case only for a reader who may see it: a
	// private project's request is left off the list of everyone outside the project, since its
	// names and its reason are that project's (maySeeRecord). The reader's own rows are read all at
	// once, within the list's clock for them, listOwnRowsBound from here: a request Dispatch cannot
	// answer for, or has not answered for when that clock runs out, is left off, failing closed for
	// that row alone, and logged (logRowUnanswered), and every other row is listed, so one project
	// Dispatch is slow to answer, or never answers, costs the list the rows that name it and no
	// other. A row whose responsible person is someone else is listed only where that person may
	// not see it (listedInStead), within a clock of those rows' own, listFallbackBound
	// from here, so that however many people's rows touch a project, and however slowly Dispatch
	// answers, they cannot keep the list from its caller. Such a row left off because a read
	// failed is logged for itself, naming its responsible person (logFallbackUnread); the rows left
	// off once the clock has run out are counted in one line for the list (logFallbackCut), since
	// each of them is on its responsible person's list wherever that person may see it, which is
	// where it belongs, and a line for each would bury the row a failed read leaves on nobody's
	// list. Those rows are read one after another, beside the reader's own. A list whose caller
	// went away (an Inbox tab or a page closed) is read by nobody, so the rows it leaves off then
	// are neither counted nor logged: its clocks did not run out. Every question the list's rows
	// ask is asked once for the list, and the list asks Dispatch at most MaySeeAtOnce questions at
	// a time (withAccessMemo), so a project many rows name, an issue key a stored reason names
	// among them, costs the list one read.
	ctx = withAccessMemo(ctx)
	fallback, cancel := context.WithTimeout(ctx, listFallbackBound)
	defer cancel()
	own, cancelOwn := context.WithTimeout(ctx, listOwnRowsBound)
	defer cancelOwn()
	insteadOf := func(i int) string {
		if notify := reads[i].notify; notify != "" && notify != record.CanonicalLogin(approver) {
			return notify
		}
		return ""
	}
	verdicts := make([]rowVerdict, len(out))
	var judging sync.WaitGroup
	judging.Go(func() {
		for i, p := range out {
			if notify := insteadOf(i); p.Kind == record.KindAgentSecret && notify != "" {
				verdicts[i].listed, verdicts[i].err = m.listedInStead(fallback, p, approver, notify, reads[i].sessionProjects, reads[i].reason)
			}
		}
	})
	for i, p := range out {
		if p.Kind == record.KindAgentSecret && insteadOf(i) == "" {
			judging.Go(func() {
				verdicts[i].listed, verdicts[i].err = m.maySeeRecord(own, approver, p.Identifiers, reads[i].sessionProjects, reads[i].reason)
			})
		}
	}
	judging.Wait()
	visible := out[:0]
	cut := 0
	for i, p := range out {
		if p.Kind != record.KindAgentSecret {
			visible = append(visible, p)
			continue
		}
		notify := insteadOf(i)
		instead := notify != ""
		listed, err := verdicts[i].listed, verdicts[i].err
		switch {
		case err != nil && ctx.Err() != nil:
			// The caller went away: neither clock ran out nor Dispatch failed to answer, so the
			// row is left off a list nobody reads, and nothing is counted or logged.
		case err != nil && instead && fallback.Err() != nil:
			cut++
		case err != nil && instead:
			m.logFallbackUnread(p.RecordID, notify, err)
		case errors.Is(err, ErrProjectAccessUnavailable), err != nil && own.Err() != nil:
			m.logRowUnanswered("pending "+p.RecordID, err, "list", "pending", "record", p.RecordID)
		case err != nil:
			return nil, err
		case listed:
			visible = append(visible, p)
		}
	}
	if cut > 0 {
		m.logFallbackCut(record.CanonicalLogin(approver), cut, fallback.Err())
	}
	return visible, nil
}

// listFallbackBound is how long after a pending list's rows are read its fallback reads, for the
// rows that name another person as responsible (listedInStead), may still start or run. It is a
// clock from that moment, not a budget of their own: those rows are read one after another, so
// however many there are they never take the list past it. Dispatch's Inbox waits 5 seconds for
// the whole list (agentsecrets.Client's defaultTimeout), and each read of Dispatch may take up to
// the project-access client's own 10, so without the clock a slow Dispatch and many other people's
// rows could empty every Inbox. A row the clock cuts short is left off, as every such row was
// before the fallback existed, and counted in the list's one line (logFallbackCut).
const listFallbackBound = 2 * time.Second

// listOwnRowsBound is how long after a list's rows are read the reads of the reader's own rows,
// all made at once, may still run: the pending list's rows that name no other person as
// responsible, and every grant the grant list asks Dispatch about. Dispatch's Inbox waits 5 seconds
// for the whole list (agentsecrets.Client's defaultTimeout), and one read of Dispatch may take up
// to the project-access client's own 10, so without it one project Dispatch does not answer would
// keep every row of the list from its caller on every poll. Three seconds leaves the other two for
// the rows' own read of the database before it and the answer's way back to Dispatch, and holds
// the densest stored reason, 80 projects, six times over at 100 ms an answer (MaySeeAtOnce). A row
// whose reads have not answered when it runs out is left off and logged (logRowUnanswered); a row
// whose projects the list or the client had already answered is still listed.
const listOwnRowsBound = 3 * time.Second

// rowVerdict is what a list read of one row: whether its reader may see it, or why that could not
// be told.
type rowVerdict struct {
	listed bool
	err    error
}

// listedInStead answers whether reader's list holds the agent-secret row p, whose record names
// notify, another person, as responsible for the agent that made it. Such a row is that person's
// (responsiblePerson), so a person who has nothing to do with the agent is not asked. Where that
// person may not see it (it belongs to a private project they are not on, from when it was made
// or since), it is listed for every other reader who may see it (maySeeRecord), so a request no
// longer waits where nobody sees it. Both reads run on fallback, the clock of such rows
// (listFallbackBound), the reader's first, so a row the reader may not see costs no read of the
// other person's. A row touching no project asks Dispatch nothing. Its error is the first read's
// that failed, or the clock's when the clock ran out before the first read; either way the row
// is left off, as it was before the fallback, failing closed. The caller tells the two apart by
// the clock: a read that failed once the clock had run out may be one the clock cut short, and
// is counted with the rows the clock left unread.
func (m *Machine) listedInStead(fallback context.Context, p PendingSummary, reader, notify string, sessionProjects []string, reason string) (bool, error) {
	if err := fallback.Err(); err != nil {
		return false, err
	}
	mine, err := m.maySeeRecord(fallback, reader, p.Identifiers, sessionProjects, reason)
	if err != nil || !mine {
		return false, err
	}
	theirs, err := m.maySeeRecord(fallback, notify, p.Identifiers, sessionProjects, reason)
	if err != nil {
		return false, err
	}
	return !theirs, nil
}

// requestedIdentifiers is what the record whose canonical body is canonical asks for, and the
// signed reason it asks with, read from the request object it embeds once that object verifies
// for this broker: its audience, its signature and its time against when the record was written.
// The reason is what the person rule reads of the row (maySeeRecord), so it comes only from an
// object that verified.
func (m *Machine) requestedIdentifiers(canonical string, createdAt time.Time) (identifiers []string, reason string, _ error) {
	body, err := record.ParseBody(canonical)
	if err != nil {
		return nil, "", err
	}
	obj, err := record.VerifyRequestObject(body.Request, m.Audience, m.Skew, createdAt)
	if err != nil {
		return nil, "", err
	}
	identifiers = make([]string, len(obj.Details))
	for i, d := range obj.Details {
		identifiers[i] = d.Identifier
	}
	return identifiers, obj.Reason, nil
}

// leftOutRetention is how long past the last list that left a record out, and past the record's
// own expiry, leaveOut remembers that it logged it. The list reads a pending record until the
// sweeper ends it, which is after its expires_at, by up to BROKER_SWEEP_SECONDS (at most 60 s)
// on a working sweeper, and the lists come every 15 s from each open Dispatch tab
// (pendingForApproverQuery's doc comment). Ten minutes is far above both, so a record still being
// listed is warned about once; a record listed again after ten quiet minutes is warned about once
// more, which is news rather than noise.
const leftOutRetention = 10 * time.Minute

// leaveOut logs PendingLeftOutMessage for the record id the first time a list leaves it out, and
// remembers it until the later of its expiry and leftOutRetention after this list, so a record
// past its expiry that the sweeper has not ended yet is not warned about on every list. The
// approval page polls every 15 s in every open tab, so a line per list would bury the one thing
// it says. Each call first forgets the records past that time, which bounds what is kept by the
// records left out within one pending window and the retention after it.
func (m *Machine) leaveOut(id, kind string, expires time.Time, reason error) {
	now := time.Now()
	keep := now.Add(leftOutRetention)
	if expires.After(keep) {
		keep = expires
	}
	m.leftOutMu.Lock()
	for logged, until := range m.leftOut {
		if now.After(until) {
			delete(m.leftOut, logged)
		}
	}
	_, logged := m.leftOut[id]
	if m.leftOut == nil {
		m.leftOut = map[string]time.Time{}
	}
	m.leftOut[id] = keep
	m.leftOutMu.Unlock()
	if !logged {
		slog.Warn(PendingLeftOutMessage, "record_id", id, "kind", kind, "reason", reason)
	}
}

// RecordDecision is a decided credential-request record's own terminal event, for the broker's
// approval page and POST /v1/machine-logins/lookup's "decided" field.
type RecordDecision struct {
	Event        string
	At           time.Time
	CredentialID string
	// Login is the person who decided the record, empty for an event no person made (expired,
	// cancelled), and empty for the model judge's own approval, which is no person's
	// (decider.eventLogin).
	Login string
	// Actor is the audit actor the event was written under: record.HumanActor for a person's
	// decision, record.JudgeActor for one the model judge made, and the broker's own actor for an
	// event it ended on its own. It is the one field that tells the judge's decision from a
	// person's, since a judged event carries no login at all, so every surface that says who
	// decided a record reads it beside Login (record.JudgeLogin).
	Actor string
	// Detail is what the decision records beside its event, empty when it records nothing. An
	// agent_secret approval carries one exactly when ApplyDecision recorded the request denied
	// rather than granted: because its requesting enrollment was no longer live, or because the
	// secret's requesters tag no longer listed the session, which the broker denies in the
	// approver's place (requests.PolicyDeniedActor).
	Detail string
}

// RecordDetail is a full credential-request record for the broker's approval page: its
// decoded request object plus its current state, folding in a later grant revocation the record's
// own decision events never overwrite (State becomes "revoked" while Decided keeps the original
// approval). Enrollment is nil for a machine login (Kind == "launcher_credential"), which carries
// no requesting enrollment at all.
type RecordDetail struct {
	RecordID        string
	Kind            string
	State           string
	Approver        string
	Enrollment      *record.Enrollment
	Identifiers     []string
	Service         string
	Reason          string
	LifetimeSeconds *int
	RulesVersion    string
	ExpiresAt       time.Time
	RequestedAt     time.Time
	Decided         *RecordDecision
	Session         Session
	// SessionProjects are the Dispatch projects the credentials the request has had name: the
	// enrollment that asked, which the record names, and the one the request hangs off now, which
	// an adoption rewrites (credentialsProjects). SecretProjects is the project of each requested
	// secret the policy tags with one, as the person rule read it (ReadRecordFor, which alone fills
	// it; ReadRecord leaves it empty). Both are empty for a record touching no project. The
	// approval page shows them.
	SessionProjects []string
	SecretProjects  []SecretProject
	// JudgeDeciding is whether the model judge holds the record's request now: pending, with its
	// judge_until ahead. No person decides such a record until the judge has given it up.
	JudgeDeciding bool
}

// ReadRecord reads a credential-request record's full detail by id, for the broker's approval page
// and, reused verbatim, POST /v1/machine-logins/lookup.
// pgx.ErrNoRows means no such record.
//
// A record nobody has decided reads "pending" until its deadline and "expired" from then on, with
// Decided nil in both, by recordStateSQL: so a record past its deadline that the sweep has not
// ended yet reads as what every decision of it already answers, rather than as one a person may
// still approve, on the approval page and in the machine-login lookup alike. Once the sweep ends
// it, its expired event (a secret request's expired row) says the same. A cancelled record keeps
// reading "cancelled", whenever the cancel came. ExpiresAt is the deadline recordStateSQL reads
// (recordDeadlineSQL): a secret request's pending_expires_at, and a machine login's expires_at.
func (m *Machine) ReadRecord(ctx context.Context, recordID string) (RecordDetail, error) {
	return m.readRecord(ctx, recordID, nil)
}

// readRecord is ReadRecord, and, where shown is not nil, ReadRecordFor: its one statement reads,
// beside the record, everything the person rule reads of it, the very column list readRowFacts
// reads under recordKey for HideUnseen (rowFactsColumns), and hands those facts to shown before
// anything else of the record is read or verified. An error shown answers is readRecord's; the
// projects it answers are the record's SecretProjects, in the order its request names its secrets.
func (m *Machine) readRecord(ctx context.Context, recordID string, shown func(rowFacts) ([]SecretProject, error)) (RecordDetail, error) {
	var canonical, requestSession, enrollmentSession, state string
	var createdAt, expiresAt time.Time
	var judgeDeciding bool
	var read rowFactsRead
	if err := m.Store.Pool.QueryRow(ctx, `select cr.body, cr.created_at, `+recordDeadlineSQL+`,
			coalesce(req.session_id, ''), coalesce(enr.session_id, ''), coalesce(`+recordStateSQL+`, 'pending'),
			coalesce(req.state = 'pending' and req.judge_until > now(), false), `+rowFactsColumns(recordKey)+`
		from `+recordKey.from+rowFactsJoins(recordKey)+`
		where `+recordKey.where, recordID).
		Scan(append([]any{&canonical, &createdAt, &expiresAt, &requestSession, &enrollmentSession, &state, &judgeDeciding}, read.dest()...)...); err != nil {
		return RecordDetail{}, err
	}
	facts := read.done(m)
	kind, approver, sessionProjects := facts.kind, deref(facts.approver), facts.sessionProjects
	var secrets []SecretProject
	if shown != nil {
		var err error
		if secrets, err = shown(facts); err != nil {
			return RecordDetail{}, err
		}
	}
	// A record the judge holds reads pending, with JudgeDeciding saying who decides it: the page
	// and the lookup show an undecided record as pending or expired, and the hold beside it.
	if state == "held" {
		state = "pending"
	}
	body, err := record.ParseBody(canonical)
	if err != nil {
		return RecordDetail{}, err
	}
	obj, err := record.VerifyRequestObject(body.Request, m.Audience, m.Skew, createdAt)
	if err != nil {
		return RecordDetail{}, err
	}
	agentSecret := kind == record.KindAgentSecret
	var enr *record.Enrollment
	if agentSecret {
		e := body.Enrollment
		enr = &e
	}
	identifiers := make([]string, len(obj.Details))
	service := ""
	for i, d := range obj.Details {
		identifiers[i] = d.Identifier
		if d.Service != "" {
			service = d.Service
		}
	}
	detail := RecordDetail{
		RecordID: recordID, Kind: kind, State: state, Approver: approver, Enrollment: enr,
		Identifiers: identifiers, Service: service, Reason: obj.Reason, LifetimeSeconds: body.LifetimeSeconds,
		RulesVersion: body.RulesVersion, ExpiresAt: expiresAt, RequestedAt: createdAt,
		Session: Session{Request: requestSession, Enrollment: enrollmentSession}, SessionProjects: sessionProjects,
		JudgeDeciding: judgeDeciding,
	}
	for _, name := range identifiers {
		if i := slices.IndexFunc(secrets, func(p SecretProject) bool { return p.Name == name }); i >= 0 {
			detail.SecretProjects = append(detail.SecretProjects, secrets[i])
		}
	}
	// Whether the record is still undecided follows PendingForApprover's rule: an agent_secret
	// record by its request's state, a machine login by its terminal event. A decided record's
	// terminal event names the decision; a request an ended enrollment cancelled before
	// endEnrollment wrote that event has none, and its request row stands in for it. The actor is
	// the event's own and has no stand-in there: a record with no event was decided by nobody, so
	// there is nobody to name, and the empty actor says so. An undecided record keeps the state
	// read above: pending, or expired past its deadline. A record cancelled after its deadline
	// reads expired there as well, and the read below finds it and names it cancelled.
	var event, credID, login, actor, eventDetail string
	var decidedAt time.Time
	if agentSecret {
		err = m.Store.Pool.QueryRow(ctx, `select coalesce(ev.event, r.state), coalesce(ev.at, r.decided_at), coalesce(ev.credential_id, ''),
				coalesce(ev.login, ''), coalesce(ev.actor, ''), coalesce(ev.detail, r.decision_detail, '')
			from requests r left join credential_request_events ev on ev.record_id=r.record_id and ev.event = any($2)
			where r.record_id=$1 and r.state <> 'pending'`, recordID, record.TerminalEventNames()).Scan(&event, &decidedAt, &credID, &login, &actor, &eventDetail)
	} else {
		err = m.Store.Pool.QueryRow(ctx, `select event, at, coalesce(credential_id,''), coalesce(login, ''), coalesce(actor, ''), coalesce(detail, '') from credential_request_events
			where record_id=$1 and event = any($2)`, recordID, record.TerminalEventNames()).Scan(&event, &decidedAt, &credID, &login, &actor, &eventDetail)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return detail, nil
	case err != nil:
		return RecordDetail{}, err
	}
	detail.State = event
	detail.Decided = &RecordDecision{Event: event, At: decidedAt, CredentialID: credID, Login: login, Actor: actor, Detail: eventDetail}
	var revoked int
	err = m.Store.Pool.QueryRow(ctx, `select 1 from credential_request_events where record_id=$1 and event='revoked' limit 1`, recordID).Scan(&revoked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return RecordDetail{}, err
	default:
		detail.State = "revoked"
	}
	return detail, nil
}

// ApproverGrant is one live grant a person may revoke, for GET /v1/grants?approver=<login>.
// Granted is policy.Automatic for a grant the policy gave the session without asking, which has no
// RecordID and an empty Approver, and policy.Approval for one a person approved, whose RecordID is
// the record the approval rests on and whose Approver is that person's login, which for an
// operator's own list can be another login. ExpiresAt is nil for a grant that lasts until its
// session ends.
//
// Approver and DecidedBy are two different facts and a judged grant tells them apart. Approver is
// the grant's own column: whose grant it is, which is what this list filters by and what
// mayRevoke reads, and for a grant the model judge approved it is the record's own approver — the
// owner, or record.AnyoneApprover for a shared secret — since the judge decides in their stead
// (D1, decider.grantApprover). DecidedBy is who decided the request the grant rests on
// (requests.decided_by): that same person for a person's own approval, and record.JudgeActor for
// the judge's, so a surface that says who decided this grant names the judge rather than a person
// who never saw the request. It is empty for an automatic grant, which nobody decided.
type ApproverGrant struct {
	GrantID    string
	Granted    string
	RecordID   *string
	Enrollment record.Enrollment
	Names      []string
	Approver   string
	DecidedBy  string
	ExpiresAt  *time.Time
	CreatedAt  time.Time
}

// GrantsForApprover lists every live grant approver may revoke, newest first: each grant of a
// session approver operates, approved or automatic, each grant approver approved on anyone's
// session, and each grant a group approver approved whose group's last good read lists approver,
// however old that read is (grantApprover records the group, and mayRevoke admits the same
// members, so a member sees every grant they may end, a judged one of a stale group included).
//
// It also lists every live grant that is on no other person's page at all: one a service's machine
// login holds, with no approver because the policy gave it without asking and no operator because
// the pod it enrolled acts for no person. That is a pod whose login carried no person
// (enrollments.operator), and a person's pod that BROKER_POD_OPERATORS no longer maps to the
// person its login carried (operatorOf). Without this a pod could take a shared agent-tier secret
// and nobody would see it. Every signed-in person the person rule shows such a grant to sees it.
// Who may end it is a separate rule, the machine login's: a service's login every signed-in person
// lists and revokes, which no person approved (enroll.Service.LiveCredentials, record.MayDecide),
// while a person's pod login is the person's BROKER_POD_OPERATORS maps the account to now, and
// theirs alone to revoke, whatever the grant names. The sentinel record.AnyoneApprover and an
// empty login are no signed-in person and list nothing. No row that already belongs to somebody
// moves: a grant with a person's approver, or on a person's own session (a mapped person's pod
// included), is listed for that person and no one else, exactly as before.
//
// It lists, to every signed-in person in the same way, every live grant whose approver is the
// sentinel record.AnyoneApprover: the model judge approved it in anyone's stead, so the grant
// records anyone as its approver and names no person at all, and such a grant of a shared secret
// can sit on a service's pod, whose enrollment has no operator either. Without this nobody would
// see what the judge had approved. That is the reasoning the paragraph above already gives —
// every signed-in person sees these — over the grants anyone may approve in the first place, so
// the people who see one are the people who could have approved it themselves.
//
// A pod whose account BROKER_POD_OPERATORS maps to a person is that person's own session here:
// its enrollment's operator is the person its login was made for, so its grants are listed to
// that person alone. The list selects a pod's grants by the enrollment row's operator, so the
// person still finds them after the setting drops the entry or maps it to another person, but from
// then on the pod acts for no person (unmappedPersonPod), so the grant no longer names them as its
// operator (operatorOf): they see each such grant they may see, and end it only as its approver or,
// where its approver is anyone, as any signed-in person may, and a grant of a private project they
// are not on is hidden from them as from anyone outside it.
//
// The enrollment each grant is listed under is joined on the grants row's own enrollment_id, so a
// grant that followed its session to another box is listed once, naming the box the session runs
// on now. A grant is listed to its approver and to the person that enrollment acts for whatever
// project it touches (namedOnGrant, operatorOf), as RevokeByApprover lets them end it whatever
// Dispatch answers: a grant
// of a secret no project tags goes on releasing after its person leaves a project its session
// names, and its own people must still see it to end it. Whether any other reader may see it reads
// the projects of both credentials its request has had, that one and the enrollment that asked
// (credentialsProjects), as every reader of whose request this is does.
func (m *Machine) GrantsForApprover(ctx context.Context, approver string) ([]ApproverGrant, error) {
	login := record.CanonicalLogin(approver)
	if login == "" || login == record.AnyoneApprover {
		return nil, nil
	}
	// The everyone arm takes a grant with no approver of a session a service's machine login
	// enrolled that acts for no person: one with no operator, or a person's pod BROKER_POD_OPERATORS
	// no longer maps to the operator its login carried (unmappedPersonPod). The mapping is the
	// broker's setting, not a table, so it goes in as two parameters, each mapped account and the
	// person it maps to, folded as record.CanonicalLogin folds them; the enrollment's operator is
	// stored folded (a launcher credential's operator is written through CanonicalLogin, and an
	// enrollment takes its credential's), and translate folds it again as fold.ASCII does, so the
	// query and operatorOf agree on which pods are mapped, and the loop below drops no row it
	// returns.
	var mappedAccounts, mappedPersons []string
	for account, person := range m.PodOperators {
		if person = record.CanonicalLogin(person); person != "" {
			mappedAccounts = append(mappedAccounts, account)
			mappedPersons = append(mappedPersons, person)
		}
	}
	rows, err := m.Store.Pool.Query(ctx, `select g.id, r.record_id, e.kind, e.runtime_id, coalesce(e.operator,''), e.slot, coalesce(g.approver,''), coalesce(r.decided_by,''), g.expires_at, g.created_at,
		coalesce((select array_agg(rs.name order by rs.name) from request_secrets rs where rs.request_id=r.id and rs.decision<>'deny'), '{}'),
		coalesce(e.projects, '{}'), e.subject, r.reason, `+credentialColumns+`
		from grants g
		join requests r on r.id=g.request_id
		join enrollments e on e.id=g.enrollment_id
		join launcher_credentials c on c.id=e.launcher_credential_id
		left join credential_requests cr on cr.id=r.record_id
		left join enrollments enr on enr.id=cr.enrollment_id
		where g.revoked_at is null and `+grantUnexpired("g")+`
		and (g.approver=$1 or e.operator=$1 or g.approver=$2 or g.approver = any($5::text[])
			or (g.approver is null and c.service is not null and (e.operator is null
				or (e.kind='pod' and not exists (select 1 from unnest($3::text[], $4::text[]) as mapped(account, person)
					where mapped.account=e.subject
					and mapped.person=translate(e.operator, 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz'))))))
		order by g.created_at desc`, login, record.AnyoneApprover, mappedAccounts, mappedPersons, m.groupApprovers(login, false))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApproverGrant
	type seen struct {
		sessionProjects []string
		reason          string
		operator        *string
	}
	var reads []seen
	for rows.Next() {
		var g ApproverGrant
		var s seen
		var current, asked enrollmentRow
		if err := rows.Scan(&g.GrantID, &g.RecordID, &g.Enrollment.Kind, &g.Enrollment.RuntimeID, &g.Enrollment.Operator, &g.Enrollment.Slot, &g.Approver, &g.DecidedBy, &g.ExpiresAt, &g.CreatedAt, &g.Names,
			&current.Projects, &current.Subject, &s.reason,
			&asked.Kind, &asked.Operator, &asked.Subject, &asked.Projects); err != nil {
			return nil, err
		}
		current.Kind = g.Enrollment.Kind
		if g.Enrollment.Operator != "" {
			current.Operator = &g.Enrollment.Operator
		}
		s.sessionProjects = m.credentialsProjects(asked, current)
		s.operator = m.operatorOf(current)
		// The row names the operator the revoke decides by (operatorOf), not the enrollment's
		// record: a person's pod BROKER_POD_OPERATORS no longer maps names none, so the page
		// offers Revoke only where the broker would take it, and an automatic grant of such a pod
		// reads as one that ends with its machine login. The selection above still reads the
		// record, so the pod's former person keeps finding the pod's grants.
		g.Enrollment.Operator = deref(s.operator)
		g.Granted = policy.Approval
		if g.RecordID == nil {
			g.Granted = policy.Automatic
		}
		out = append(out, g)
		reads = append(reads, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// A private project's grant is left off the list of everyone outside the project but its
	// approver and its operator (operatorOf: none for a person's pod BROKER_POD_OPERATORS no longer
	// maps), read once the rows are closed so no call to Dispatch holds the pooled connection
	// (maySeeRecord). The grants are read all at once within the list's clock (listOwnRowsBound),
	// as PendingForApprover's own rows are: a grant Dispatch cannot answer for, or has not answered
	// for when the clock runs out, is left off alone and logged, and one left off because the caller
	// went away is not logged. Each question is asked once for the list, and the list asks Dispatch
	// at most MaySeeAtOnce questions at a time (withAccessMemo), as PendingForApprover's are.
	ctx = withAccessMemo(ctx)
	own, cancelOwn := context.WithTimeout(ctx, listOwnRowsBound)
	defer cancelOwn()
	verdicts := make([]rowVerdict, len(out))
	var judging sync.WaitGroup
	for i, g := range out {
		if named, _ := namedOnGrant(login, &g.Approver, reads[i].operator); named {
			verdicts[i].listed = true
			continue
		}
		judging.Go(func() {
			verdicts[i].listed, verdicts[i].err = m.maySeeRecord(own, login, g.Names, reads[i].sessionProjects, reads[i].reason)
		})
	}
	judging.Wait()
	visible := out[:0]
	for i, g := range out {
		ok, err := verdicts[i].listed, verdicts[i].err
		switch {
		case err != nil && ctx.Err() != nil:
			// The caller went away: nothing is logged, and the grant is left off a list nobody reads.
		case errors.Is(err, ErrProjectAccessUnavailable), err != nil && own.Err() != nil:
			attrs := []any{"list", "grants", "grant", g.GrantID}
			if g.RecordID != nil {
				attrs = append(attrs, "record", *g.RecordID)
			}
			m.logRowUnanswered("grant "+g.GrantID, err, attrs...)
		case err != nil:
			return nil, err
		case ok:
			visible = append(visible, g)
		}
	}
	return visible, nil
}

// groupApprovers is the groups the group reader lists login in, each in the group form a record's
// and a grant's approver holds it in (record.GroupPrefix), and an empty list with no reader: with
// freshOnly, for PendingForApprover, the groups that admit login now (GroupsOf), since a member
// decides only while the group reads fresh; without it, for GrantsForApprover, every group whose
// last good read lists login however old (GroupsListing), since such a member may revoke the
// group's grant (mayRevoke), and a grant the model judge approved for a stale group's secret still
// releases.
func (m *Machine) groupApprovers(login string, freshOnly bool) []string {
	if m.Groups == nil {
		return []string{}
	}
	var groups []string
	if freshOnly {
		groups = m.Groups.GroupsOf(login)
	} else {
		groups = m.Groups.GroupsListing(login)
	}
	approvers := make([]string, 0, len(groups))
	for _, group := range groups {
		approvers = append(approvers, record.GroupPrefix+group)
	}
	return approvers
}
