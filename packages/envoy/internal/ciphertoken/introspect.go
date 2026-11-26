package ciphertoken

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/fold"
)

// IntrospectPath is the broker's RFC 7662 introspection route, which answers what the broker
// knows about one of its access tokens.
const IntrospectPath = "/v1/oauth/introspect"

// introspectTimeout is the window one introspection gets, matching the window every other call
// Dispatch makes to the broker gets (internal/dispatch/cipher). A request waiting on this
// check waits no longer than that for the broker.
const introspectTimeout = 5 * time.Second

// maxIntrospectionBytes caps the answer read from the broker. The answer is a dozen short fields,
// so anything larger is not the broker's own and is refused rather than held.
const maxIntrospectionBytes = 64 << 10

// Project scopes an Introspection's Projects is read under: the broker's own wire names, carried
// here so a reader of a Caller this package decides reads one package (internal/dispatch/api).
const (
	// ProjectScopeNamed means Projects is the one project an operator's entry gave the token, or
	// none for an account entry that names none, whether it names a person or acts as its account
	// (a service, no person). A job's named token, which names a service and no person, names
	// exactly one.
	ProjectScopeNamed = oauthwire.ProjectScopeNamed
	// ProjectScopePerson means the token acts as its person, and Projects is what its trade
	// asked for.
	ProjectScopePerson = oauthwire.ProjectScopePerson
	// ProjectScopeNone means the token writes no project and names none: a read-only token,
	// which holds ScopeRead and nothing else beside it (shapeOf) and which a verifier accepts
	// only under Options.ServesReadOnly.
	ProjectScopeNone = oauthwire.ProjectScopeNone
	// ProjectScopeResults means Projects is the one project whose results the token writes, for
	// the run it carries, and it writes nothing else: a results token, which holds ScopeResults
	// and nothing else beside it (shapeOf) and which a verifier accepts only under
	// Options.ServesResults.
	ProjectScopeResults = oauthwire.ProjectScopeResults
	// ProjectScopePublic means the token is a job's whose entry names public: it names a service
	// and no person, it reaches every public project, and Projects is the 0 to
	// oauthwire.MaxProjects private projects its entry lists (shapeOf).
	ProjectScopePublic = oauthwire.ProjectScopePublic
)

// ScopeRead is the one OAuth scope every read-only token holds, in the answer's space-separated
// Scope: the broker's own wire name, carried here for the reason the project scopes are. Beside
// ProjectScopeNone or a narrowed project scope it is a read-only token, and beside the project
// scope named or person it is no session's credential (ErrNotASession).
const ScopeRead = oauthwire.ScopeRead

// ScopeResults is the one OAuth scope a results token holds, carried here for the reason
// ScopeRead is. Beside ProjectScopeResults it is a results token, and beside any other project
// scope it is no session's credential (ErrNotASession).
const ScopeResults = oauthwire.ScopeResults

// CIRun is the GitHub Actions run a results token was minted for, as the broker answers it
// (Introspection.CI), carried here for the reason the project scopes are.
type CIRun = oauthwire.CIRun

// ScopeApprove is the one OAuth scope an approve-only token holds: beside ProjectScopePerson, a
// person, and no service, no projects and no cut (approveShapeOf) it is the token the broker mints
// for a person's approve grant and spends through its proxy, which a verifier accepts only under
// Options.ServesApproveOnly, and in any other shape it is no session's credential
// (ErrNotASession).
const ScopeApprove = oauthwire.ScopeApprove

// ScopeLegionStatus is the OAuth scope the broker grants a Legion daemon's own token, in the
// answer's space-separated Scope: the broker's own wire name, carried here for the reason
// ScopeRead is. callerOf reads it into Caller.LegionStatus. Beside the project scope named or
// person, shapeOf reads a token that names a person and holds it as it reads one that does not,
// but for one beside ScopeApprove: approveShapeOf reads that token first and answers it no
// session's token. A named answer that names no person and holds any scope word, this one
// included, is no session's credential (shapeOf).
const ScopeLegionStatus = oauthwire.ScopeLegionStatus

// LegionDaemonSessionPrefix starts the session a Legion daemon writes its project's issues under,
// legion-daemon:<PROJECT>: the broker's own spelling (oauthwire), carried here for the reason
// ScopeLegionStatus is.
const LegionDaemonSessionPrefix = oauthwire.LegionDaemonSessionPrefix

// ExchangeClientID is the OAuth client every token issued for acting as a session is issued to:
// the RFC 8693 exchange a pod, a workflow run or a dev box session asks for a Dispatch access
// token through (internal/cipher/oauthwire). It is carried here for the same reason the project
// scopes are: a reader of a Caller this package decides reads one package.
const ExchangeClientID = oauthwire.ExchangeClientID

// Introspection is what the broker knows about one of its access tokens, the RFC 7662 answer this
// package reads a presented token back through: the broker's own declaration of that answer,
// which its route serialises (internal/cipher/oauthwire). The two ends share one struct rather
// than a copy each, so a field added to the answer reaches this check and the compiler holds the
// names and the JSON tags together. It is named here because this package's callers decode and
// read it without the broker's storage, as this package itself does: oauthwire imports nothing of
// the broker.
type Introspection = oauthwire.Introspection

// expiryOf is when the token an answer describes stops being live, or the zero time when the
// broker named no 'exp'. It, cutOf and callerOf are functions rather than methods because the
// answer is the broker's own type, which this package may add no method to.
func expiryOf(answer Introspection) time.Time {
	if answer.ExpiresAt <= 0 {
		return time.Time{}
	}
	return time.Unix(answer.ExpiresAt, 0)
}

// cutOf is the cut an answer names (Introspection.AsOf) in UTC, or the zero time when it names
// none. A cut that is not an RFC 3339 time is an error, and so is one that reads as the zero
// time, which would read as no cut at all.
func cutOf(answer Introspection) (time.Time, error) {
	if answer.AsOf == "" {
		return time.Time{}, nil
	}
	cut, err := time.Parse(time.RFC3339Nano, answer.AsOf)
	if err != nil {
		return time.Time{}, err
	}
	if cut.IsZero() {
		return time.Time{}, fmt.Errorf("the cut %q is the zero time", answer.AsOf)
	}
	return cut.UTC(), nil
}

// callerOf is the Caller a live answer names, the one shape both checks hand back. The list of
// projects is carried whole, under any scope, as the caller's own copy, and the scope beside it
// as its base word (oauthwire.BaseProjectScope), because a reader deciding what the token may do
// needs the scope itself and not only the list, and a narrowed token's projects mean what its
// base word's do; a results answer names its one project. The service is carried as the broker
// answered it, which is set exactly where the person is not (sessionCaller refuses an answer
// naming neither). ReadOnly, ResultsOnly and ApproveOnly are shapeOf's verdict on the answer,
// which sessionCaller reached and hands over as shape, and cut is the answer's cut, which
// sessionCaller read (cutOf): callerOf runs only on an answer sessionCaller accepted, and
// sessionCaller lets a read-only, a results or an approve-only answer through only with its switch
// set, which only a verifier that serves one sets, so a Caller marked any of those ways is one
// such a verifier accepted. The run of a results answer is copied, so a reader of the Caller
// shares nothing with the answer the cache keeps for the next request. LegionStatus is whether
// Scope holds the word ScopeLegionStatus, read one word at a time as shapeOf reads it and with no
// slice built, since this too runs on every request a verifier decides.
func callerOf(answer Introspection, shape tokenShape, cut time.Time) Caller {
	caller := Caller{
		Session:      answer.Session,
		Person:       answer.Person,
		Service:      answer.Service,
		Projects:     slices.Clone(answer.Projects),
		ProjectScope: oauthwire.BaseProjectScope(answer.ProjectScope),
		ReadOnly:     shape == shapeReadOnly,
		ResultsOnly:  shape == shapeResults,
		ApproveOnly:  shape == shapeApproveOnly,
		Cut:          cut,
	}
	if shape == shapeResults {
		run := *answer.CI
		caller.CI = &run
	}
	for word := range strings.FieldsSeq(answer.Scope) {
		if word == ScopeLegionStatus {
			caller.LegionStatus = true
			break
		}
	}
	return caller
}

// introspect asks the broker what token is, presenting Dispatch's own projected ServiceAccount
// token as the bearer the broker resolves its caller from. Every failure to obtain an answer —
// the account token Dispatch could not read, a transport failure, a status the broker does not
// answer a sound request with, a body that does not decode — is ErrBrokerUnreachable, never a
// refusal: a caller told its token was refused would discard a token that may be perfectly live,
// while 503 asks it to come back.
func (v *Verifier) introspect(ctx context.Context, token string) (Introspection, error) {
	account, err := v.account.Read()
	if err != nil {
		return Introspection{}, fmt.Errorf("%w: %w", ErrBrokerUnreachable, err)
	}
	form := url.Values{"token": []string{token}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, v.brokerURL+IntrospectPath, strings.NewReader(form))
	if err != nil {
		return Introspection{}, fmt.Errorf("%w: build the introspection request: %w", ErrBrokerUnreachable, err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer "+account)
	response, err := v.http.Do(request)
	if err != nil {
		// The error carries the request's URL, which is the broker's and never the token: the
		// token is in the body, which http.Client puts in no error.
		return Introspection{}, fmt.Errorf("%w: %w", ErrBrokerUnreachable, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxIntrospectionBytes))
	if err != nil {
		return Introspection{}, fmt.Errorf("%w: read the broker's answer: %w", ErrBrokerUnreachable, err)
	}
	if response.StatusCode != http.StatusOK {
		// A 429 is the broker's own account-class bucket, a 401 or 403 Dispatch's own account
		// token, a 5xx the broker failing: none of them says anything about the token presented,
		// so none of them refuses it.
		return Introspection{}, fmt.Errorf("%w: the broker answered %d", ErrBrokerUnreachable, response.StatusCode)
	}
	var answer struct {
		Introspection
		// LegacyProject is the field a broker before the list answered a named token's one
		// project under. This reader reads no such field, so an answer carrying it would pass as
		// a named token naming no project — the public-session account's shape, which Dispatch
		// holds to no project — and is refused instead, whatever else it says.
		LegacyProject json.RawMessage `json:"project"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return Introspection{}, fmt.Errorf("%w: decode the broker's answer: %w", ErrBrokerUnreachable, err)
	}
	if answer.LegacyProject != nil {
		return Introspection{}, ErrNotASession
	}
	// The person is read in its canonical form (fold.Login, which Dispatch's model.CanonicalLogin
	// reads) here, where it is first read, so sessionCaller's checks, the cache and every Caller
	// see that form and no later reader canonicalises it again. The broker does not canonicalise
	// every source it names a person from: it lowers a Google sign-in's login, a machine's
	// approver and a pod's operator, but answers the person of a BROKER_EXCHANGE_ACCOUNTS or
	// BROKER_EXCHANGE_WORKFLOWS entry as the entry writes it. Every login Dispatch stores is
	// canonical, the owner of a row a session writes among them, so a person taken as the broker
	// spells it would be a login no other row or read names. A person that is empty once trimmed
	// names nobody, and sessionCaller reads it as it reads an absent one.
	answer.Person = fold.Login(answer.Person)
	return answer.Introspection, nil
}
