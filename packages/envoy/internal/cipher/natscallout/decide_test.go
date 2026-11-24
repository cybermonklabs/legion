package natscallout

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/machine"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// encodeAnswer is the authorization response the server reads for req, signed by issuer: the
// verdict admit settles, placed in the global account, signed, as answer runs them, for a test
// that holds a decision and no running service.
func encodeAnswer(issuer nkeys.KeyPair, req Request, decision Decision, refusal error) (string, error) {
	user, refused := admit(config.GlobalNATSAccount, req, decision, asRefusal(refusal))
	return sign(issuer, req, user, refused)
}

// session is one session's key, the access token the broker bound to it and the broker's answer
// about that token, as the auth service's Introspect would read it.
type session struct {
	id     string
	key    *ecdsa.PrivateKey
	token  string
	answer oauthwire.Introspection
}

func newSession(t testing.TB, id string, life time.Duration) *session {
	t.Helper()
	key, err := proof.NewKey()
	require.NoError(t, err)
	sum, err := (&jose.JSONWebKey{Key: key.Public()}).Thumbprint(crypto.SHA256)
	require.NoError(t, err)
	return &session{
		id: id, key: key, token: "access-token-of-" + id,
		answer: oauthwire.Introspection{
			Active: true, Session: id, Person: "person@example.com",
			ClientID: ciphertoken.ExchangeClientID, ProjectScope: ciphertoken.ProjectScopePerson,
			Thumbprint: base64.RawURLEncoding.EncodeToString(sum),
			ExpiresAt:  time.Now().Add(life).Unix(),
		},
	}
}

// request is the authorization request the server sends for this session connecting with nonce:
// its token in the CONNECT's token field and its proof over nonce in the signature field, which
// nats.go base64url-encodes.
func (s *session) request(t testing.TB, nonce string) Request {
	t.Helper()
	signed, err := cipherclient.ConnectionProof(s.key, s.token, nonce)
	require.NoError(t, err)
	return Request{
		UserNkey: "UUSER", ServerID: "NSERVER", Nonce: nonce, Token: s.token,
		Sig: base64.RawURLEncoding.EncodeToString([]byte(signed)), Host: "127.0.0.1",
	}
}

// introspector answers each session's token with its answer, and every other token not live.
func introspector(sessions ...*session) func(context.Context, string) (oauthwire.Introspection, error) {
	return func(_ context.Context, token string) (oauthwire.Introspection, error) {
		for _, s := range sessions {
			if s.token == token {
				return s.answer, nil
			}
		}
		return oauthwire.Introspection{}, nil
	}
}

// platformToken is a service's platform identity token as the stub ServiceToken reads it: the
// account it names and its life.
type platformToken struct {
	account        string
	google         bool
	issued, expiry time.Time
}

// serviceTokens verifies each raw token tokens names, and refuses every other.
func serviceTokens(tokens map[string]platformToken) func(context.Context, string) (string, bool, time.Time, time.Time, error) {
	return func(_ context.Context, raw string) (string, bool, time.Time, time.Time, error) {
		token, ok := tokens[raw]
		if !ok {
			return "", false, time.Time{}, time.Time{}, errors.New("the token did not verify")
		}
		return token.account, token.google, token.issued, token.expiry, nil
	}
}

func serviceRequest(raw string) Request {
	return Request{UserNkey: "UUSER", ServerID: "NSERVER", Nonce: "nonce", User: natscred.ServiceUser, Password: raw, Host: "10.0.0.7"}
}

// The platform accounts this package's tests list in BROKER_NATS_SERVICES.
const (
	listenerAccount = "system:serviceaccount:example-ns:example-listener"
	daemonAccount   = "system:serviceaccount:example-ns:example-daemon"
	measureAccount  = "envoy-measure@example-project.iam.gserviceaccount.com"
	edgeAccount     = "system:serviceaccount:example-ns:example-edge-listener"
)

var exampleServices = []config.NATSService{
	{Kind: config.NATSDispatch, MachineID: "example-dispatch", Account: "system:serviceaccount:dispatch:dispatch", AllProjects: true},
	{Kind: config.NATSListener, MachineID: "example-hub", Account: listenerAccount, AllProjects: true},
	{Kind: config.NATSDaemon, Account: daemonAccount, AllProjects: true},
	{Kind: config.NATSMeasure, Account: measureAccount, Google: true, AllProjects: true},
	{Kind: config.NATSDaemon, Account: "system:serviceaccount:example-ns:held-daemon"},
}

// A session's live token and a proof over this connection's nonce are answered with a user of
// that session's own: its delivery channel and agent stream's control subject to subscribe, its
// reply inboxes and its frames to publish, named session/<id>, expiring with the token.
func TestASessionTokenGetsItsOwnChannel(t *testing.T) {
	a := newSession(t, "ses_a", 15*time.Minute)
	decision, err := Decide(t.Context(), Deps{Introspect: introspector(a)}, nil, a.request(t, "nonce-of-this-connection"))
	require.NoError(t, err)
	want, err := natsgrant.Session("ses_a")
	require.NoError(t, err)
	require.Equal(t, want, decision.Grant)
	require.Equal(t, "session/ses_a", decision.Name)
	require.Equal(t, time.Unix(a.answer.ExpiresAt, 0), decision.Expires)
	require.Contains(t, decision.Grant.Subscribe, "notifications.envoy.deliver.ses_a")
	require.NotContains(t, decision.Grant.Subscribe, "notifications.envoy.deliver.*")
}

// A service's platform token, from an account the deployment lists, is answered with its kind's
// grant from natsgrant, named <kind>/<machine id or account>, expiring with the token.
func TestAServiceTokenGetsItsKindsGrant(t *testing.T) {
	now := time.Now()
	tokens := map[string]platformToken{
		"dispatch-token": {account: "system:serviceaccount:dispatch:dispatch", issued: now, expiry: now.Add(time.Hour)},
		"listener-token": {account: listenerAccount, issued: now, expiry: now.Add(time.Hour)},
		"daemon-token":   {account: daemonAccount, issued: now, expiry: now.Add(30 * time.Minute)},
		"measure-token":  {account: measureAccount, google: true, issued: now, expiry: now.Add(time.Hour)},
	}
	deps := Deps{ServiceToken: serviceTokens(tokens)}
	for raw, want := range map[string]Decision{
		"dispatch-token": {Name: "dispatch/example-dispatch", Grant: natsgrant.Dispatch("example-dispatch"), Expires: now.Add(time.Hour)},
		"listener-token": {Name: "listener/example-hub", Grant: natsgrant.Listener("example-hub"), Expires: now.Add(time.Hour)},
		"daemon-token":   {Name: "daemon/" + daemonAccount, Grant: natsgrant.Daemon(), Expires: now.Add(30 * time.Minute)},
		"measure-token":  {Name: "measure/" + measureAccount, Grant: natsgrant.Measure(), Expires: now.Add(time.Hour)},
	} {
		t.Run(raw, func(t *testing.T) {
			decision, err := Decide(t.Context(), deps, exampleServices, serviceRequest(raw))
			require.NoError(t, err)
			require.Equal(t, want, decision)
		})
	}
}

// An edge listener's entry, read as BROKER_NATS_SERVICES writes it, is answered with the edge's
// grant for its machine id and the projects the entry lists (natsgrant.Edge), named for its
// machine id and expiring with its token, and that grant passes the guard every answer passes. The
// grant publishes the role subjects of those projects and no other project's: an entry of two
// projects publishes both, an entry of one the one. It is not the listener's grant, which
// publishes every notification topic.
func TestTheAuthServiceAnswersTheEdgeItsGrant(t *testing.T) {
	now := time.Now()
	deps := Deps{ServiceToken: serviceTokens(map[string]platformToken{
		"edge-token": {account: edgeAccount, issued: now, expiry: now.Add(time.Hour)},
	})}
	for _, test := range []struct {
		entry    string
		projects []string
	}{
		{entry: "edge-listener/edge-a/PRIV=" + edgeAccount, projects: []string{"PRIV"}},
		{entry: "edge-listener/edge-a/PRIV,OTHER=" + edgeAccount, projects: []string{"OTHER", "PRIV"}},
	} {
		t.Run(test.entry, func(t *testing.T) {
			services, err := config.ParseNATSServices(test.entry)
			require.NoError(t, err)
			decision, err := Decide(t.Context(), deps, services, serviceRequest("edge-token"))
			require.NoError(t, err)
			want, err := natsgrant.Edge("edge-a", test.projects)
			require.NoError(t, err)
			require.Equal(t, Decision{Name: "edge-listener/edge-a", Grant: want, Expires: now.Add(time.Hour)}, decision)
			for _, project := range test.projects {
				require.Contains(t, decision.Grant.Publish, "notifications.role."+project+".>")
				require.Contains(t, decision.Grant.Publish, "notifications.envoy.exceptions.notifications.role."+project+".>")
			}
			require.NotContains(t, decision.Grant.Publish, "notifications.role.INFRA.>")
			require.NotEqual(t, natsgrant.Listener("edge-a"), decision.Grant)
			require.Nil(t, guard(decision.Grant))
		})
	}
}

// An edge listener's entry whose projects natsgrant.Edge refuses - one config.ParseNATSServices would not
// have read, a wildcard among them - is answered with no grant, which the guard refuses as an open
// direction, and never with a grant built of that list.
func TestAnEdgeEntryWhoseProjectsEdgeRefusesGetsNoGrant(t *testing.T) {
	grant := serviceGrant(config.NATSService{Kind: config.NATSEdgeListener, MachineID: "edge-a", Account: edgeAccount, Projects: []string{"*"}})
	require.Equal(t, natsgrant.Permissions{}, grant)
	refused := guard(grant)
	require.NotNil(t, refused)
	require.Equal(t, labelOpenDirection, refused.Label)
}

// Every refusal is one line naming why, carries nothing of the credential, and is counted under
// its own fixed label, the decision counter's reason. A row whose ctx is set decides under that
// context instead of the test's: a spent one is a decision past its budget.
func TestRefusals(t *testing.T) {
	now := time.Now()
	a := newSession(t, "ses_a", 15*time.Minute)
	expired := newSession(t, "ses_expired", 15*time.Minute)
	expired.answer.Active = false
	dotted := newSession(t, "a.b", 15*time.Minute)
	other := newSession(t, "ses_other", 15*time.Minute)
	otherKey := *a
	otherKey.key = other.key
	// A live token the broker issued to a client other than the exchange every session's token
	// comes from is no session's credential (ciphertoken.SessionCaller).
	foreign := newSession(t, "ses_foreign", 15*time.Minute)
	foreign.answer.ClientID = "some-other-client"
	unbounded := newSession(t, "ses_unbounded", 15*time.Minute)
	unbounded.answer.ExpiresAt = 0
	// A read-only token, the one Dispatch alone serves (ciphertoken.Options.ServesReadOnly), is no
	// session's credential to the auth service, which reads every answer with that switch off
	// (ciphertoken.SessionCaller): the project scope none beside the one scope read, a service,
	// and no person and no project.
	readOnly := newSession(t, "ses_reader", 15*time.Minute)
	readOnly.answer.Person, readOnly.answer.Service = "", "example-reader"
	readOnly.answer.ProjectScope, readOnly.answer.Scope = ciphertoken.ProjectScopeNone, ciphertoken.ScopeRead
	// A token narrowed to reading is read-only too, whatever project scope it narrows, so each
	// answer of the golden file the broker's own test holds its narrowed answers to is no
	// session's credential to the auth service either: each is a session of its own, presenting
	// its token with a sound proof.
	kinds := slices.Sorted(maps.Keys(ciphertokentest.NarrowedAnswers(t, "", "", 0)))
	narrowed := make([]*session, 0, len(kinds))
	for _, kind := range kinds {
		s := newSession(t, "ses_narrowed_"+strings.ReplaceAll(kind, "-", "_"), 15*time.Minute)
		s.answer = ciphertokentest.NarrowedAnswers(t, s.answer.Thumbprint, s.id, s.answer.ExpiresAt)[kind]
		narrowed = append(narrowed, s)
	}
	tokens := map[string]platformToken{
		"unlisted-token":  {account: "system:serviceaccount:example-ns:stranger", issued: now, expiry: now.Add(time.Hour)},
		"two-hours-token": {account: daemonAccount, issued: now, expiry: now.Add(2 * time.Hour)},
		"held-token":      {account: "system:serviceaccount:example-ns:held-daemon", issued: now, expiry: now.Add(time.Hour)},
	}
	introspected := append([]*session{a, expired, dotted, foreign, unbounded, readOnly}, narrowed...)
	deps := Deps{Introspect: introspector(introspected...), ServiceToken: serviceTokens(tokens)}
	unreachable := Deps{Introspect: func(context.Context, string) (oauthwire.Introspection, error) {
		return oauthwire.Introspection{}, errors.New("dial tcp: connection refused")
	}}
	// cancelled is what each of the broker's calls answers once the decision's context ends.
	cancelled := Deps{
		Introspect: func(ctx context.Context, _ string) (oauthwire.Introspection, error) {
			return oauthwire.Introspection{}, ctx.Err()
		},
		ServiceToken: func(ctx context.Context, _ string) (string, bool, time.Time, time.Time, error) {
			return "", false, time.Time{}, time.Time{}, ctx.Err()
		},
	}
	spent, cancel := context.WithCancel(context.Background())
	cancel()
	badSig := a.request(t, "nonce")
	badSig.Sig = "%%%"
	type refusalRow struct {
		name   string
		deps   Deps
		ctx    context.Context
		req    Request
		label  string
		reason string
	}
	rows := []refusalRow{
		{"no credential", deps, nil, Request{UserNkey: "UUSER", ServerID: "NSERVER"}, labelNoCredential, "no credential this hub admits"},
		{"a user and no password", deps, nil, Request{UserNkey: "UUSER", User: natscred.ServiceUser}, labelNoCredential, "no credential this hub admits"},
		{"a token and no proof", deps, nil, Request{UserNkey: "UUSER", Token: a.token}, labelNoCredential, "no credential this hub admits"},
		{"a token that is not live", deps, nil, expired.request(t, "nonce"), labelTokenNotLive, "token not live"},
		{"a proof of another key", deps, nil, otherKey.request(t, "nonce"), labelKeyMismatch, "key mismatch"},
		{"a live token that is no session's credential", deps, nil, foreign.request(t, "nonce"), labelNotASession, "token is no session's credential"},
		{"a read-only token", deps, nil, readOnly.request(t, "nonce"), labelNotASession, "token is no session's credential"},
		{"a proof that is no base64url", deps, nil, badSig, labelProofInvalid, "proof invalid: jws"},
		{"a token that names no expiry", deps, nil, unbounded.request(t, "nonce"), labelNoExpiry, "token names no expiry"},
		{"a session id that is not one subject token", deps, nil, dotted.request(t, "nonce"), labelBadSessionID, "session id is not one subject token"},
		{"a broker that does not answer", unreachable, nil, a.request(t, "nonce"), labelBrokerUnreachable, "broker unreachable"},
		{"a budget spent in the token's introspection", cancelled, spent, a.request(t, "nonce"), labelBudget, "decision exceeded its budget at the token's introspection"},
		{"a service token that does not verify", deps, nil, serviceRequest("forged"), labelServiceToken, "service token not verified"},
		{"a budget spent in the service token's verification", cancelled, spent, serviceRequest("held-token"), labelBudget, "decision exceeded its budget at the service token's verification"},
		{"an unlisted service", deps, nil, serviceRequest("unlisted-token"), labelServiceNotListed, "service not listed: system:serviceaccount:example-ns:stranger"},
		{"a two-hour platform token", deps, nil, serviceRequest("two-hours-token"), labelTokenTooLong, "token lives longer than an hour"},
		{"a service held to projects this broker cannot read", deps, nil, serviceRequest("held-token"), labelHeldToProjects, "held to exchange projects, which this broker cannot read: service daemon/system:serviceaccount:example-ns:held-daemon"},
	}
	for i, s := range narrowed {
		rows = append(rows, refusalRow{"a token narrowed from a " + kinds[i], deps, nil, s.request(t, "nonce"), labelNotASession, "token is no session's credential"})
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			ctx := row.ctx
			if ctx == nil {
				ctx = t.Context()
			}
			_, err := Decide(ctx, row.deps, exampleServices, row.req)
			var refused *Refusal
			require.True(t, errors.As(err, &refused), "%v", err)
			require.Equal(t, row.label, refused.Label)
			require.True(t, strings.HasPrefix(refused.Reason, row.reason), "%q does not begin %q", refused.Reason, row.reason)
			require.NotContains(t, refused.Reason, "\n")
			for _, secret := range []string{row.req.Token, row.req.Sig, row.req.Password} {
				if secret != "" {
					require.NotContains(t, refused.Reason, secret)
				}
			}
		})
	}
	require.Equal(t, machine.MaxTokenLoginLifetime, time.Hour, "the refusal's text names the bound")
}

// An error of the service's own, one that is no *Refusal, is refused as the service's failure,
// under its own label, and its text is the log line's cause, never the answer's reason: such an
// error can name the broker's own addresses. A *Refusal is answered as it is.
func TestAFailureOfTheServiceIsRefusedNamingNothingOfIt(t *testing.T) {
	cause := errors.New("dial tcp 10.0.0.9:5432: connect: connection refused")
	refused := asRefusal(cause)
	require.Equal(t, labelServiceFailed, refused.Label)
	require.Equal(t, "the service failed", refused.Reason)
	require.Equal(t, cause, refused.Cause)
	own := refuse(labelKeyMismatch, "key mismatch")
	require.Same(t, own, asRefusal(fmt.Errorf("decide: %w", own)))
	require.Nil(t, asRefusal(nil))
}

// The user the service answers with expires when the credential it was issued for does, read
// from the encoded answer the server decodes: a session's token's expiry and a service's
// platform token's.
func TestTheUserExpiresWithItsCredential(t *testing.T) {
	issuer, err := nkeys.CreateAccount()
	require.NoError(t, err)
	user, err := nkeys.CreateUser()
	require.NoError(t, err)
	userKey, err := user.PublicKey()
	require.NoError(t, err)
	server, err := nkeys.CreateServer()
	require.NoError(t, err)
	serverID, err := server.PublicKey()
	require.NoError(t, err)

	now := time.Now()
	a := newSession(t, "ses_a", 7*time.Minute)
	tokens := map[string]platformToken{"daemon-token": {account: daemonAccount, issued: now, expiry: now.Add(40 * time.Minute)}}
	deps := Deps{Introspect: introspector(a), ServiceToken: serviceTokens(tokens)}
	for name, row := range map[string]struct {
		req    Request
		expiry int64
	}{
		"session": {a.request(t, "nonce"), a.answer.ExpiresAt},
		"service": {serviceRequest("daemon-token"), now.Add(40 * time.Minute).Unix()},
	} {
		t.Run(name, func(t *testing.T) {
			row.req.UserNkey, row.req.ServerID = userKey, serverID
			decision, err := Decide(t.Context(), deps, exampleServices, row.req)
			require.NoError(t, err)
			encoded, err := encodeAnswer(issuer, row.req, decision, nil)
			require.NoError(t, err)
			response, err := jwt.DecodeAuthorizationResponseClaims(encoded)
			require.NoError(t, err)
			require.Equal(t, serverID, response.Audience)
			admitted, err := jwt.DecodeUserClaims(response.Jwt)
			require.NoError(t, err)
			require.Equal(t, row.expiry, admitted.Expires, "the user expires at %s, its credential at %s",
				time.Unix(admitted.Expires, 0), time.Unix(row.expiry, 0))
			require.Equal(t, config.GlobalNATSAccount, admitted.Audience)
			require.Equal(t, decision.Name, admitted.Name)
			require.Equal(t, userKey, admitted.Subject)
		})
	}
}

// A proof signs the nonce of the connection it was made for, so one captured from that
// connection's CONNECT and presented on another, which the server sends another nonce, is
// refused: the replay store the HTTP check keeps is not needed here.
func TestAProofCapturedFromOneConnectionIsRefusedOnAnother(t *testing.T) {
	a := newSession(t, "ses_a", 15*time.Minute)
	deps := Deps{Introspect: introspector(a)}
	first := a.request(t, "nonce-of-the-first-connection")
	_, err := Decide(t.Context(), deps, nil, first)
	require.NoError(t, err)

	replayed := first
	replayed.Nonce = "nonce-of-the-second-connection"
	_, err = Decide(t.Context(), deps, nil, replayed)
	var refused *Refusal
	require.True(t, errors.As(err, &refused), "the second connection, presenting the first's proof, was admitted: %v", err)
	require.Equal(t, "proof invalid: nonce", refused.Reason)
}

// Every answer passes the guard in admit, whatever decided it: a grant naming a system subject is
// answered with a refusal and no user. The server takes from a callout client only the publish of
// $SYS.REQ.USER.AUTH, so a grant naming one would hand a connection a subscribe to the
// authorization requests. The control row is the same grant without the system subject, admitted,
// so the refusal is the subject's.
func TestAGrantNamingASystemSubjectIsRefused(t *testing.T) {
	issuer, err := nkeys.CreateAccount()
	require.NoError(t, err)
	user, err := nkeys.CreateUser()
	require.NoError(t, err)
	userKey, err := user.PublicKey()
	require.NoError(t, err)
	for _, row := range []struct {
		name      string
		subscribe []string
		refusal   string
	}{
		{"a subscribe to the authorization subject", []string{"_INBOX.>", "$SYS.REQ.USER.AUTH"}, "grant names a system subject: $SYS.REQ.USER.AUTH"},
		{"a subscribe to every system subject", []string{"_INBOX.>", "$SYS.>"}, "grant names a system subject: $SYS.>"},
		// nats-server reads the entry as > in the auth service's queue group, splitting at the tab
		// (strings.Fields, server/client.go splitSubjectQueue).
		{"every subject in the auth service's queue group, after a tab", []string{"_INBOX.>", ">\tenvoy-auth"}, "grant names a system subject: >\tenvoy-auth"},
		{"control: the same grant without one", []string{"_INBOX.>"}, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			grant := natsgrant.Permissions{Publish: []string{"_INBOX.>"}, Subscribe: row.subscribe}
			encoded, err := encodeAnswer(issuer, Request{UserNkey: userKey, ServerID: "NSERVER"},
				Decision{Name: "session/ses_a", Grant: grant, Expires: time.Now().Add(time.Hour)}, nil)
			require.NoError(t, err)
			response, err := jwt.DecodeAuthorizationResponseClaims(encoded)
			require.NoError(t, err)
			require.Equal(t, row.refusal, response.Error)
			_, refused := admit(config.GlobalNATSAccount, Request{UserNkey: userKey}, Decision{Grant: grant}, nil)
			if row.refusal != "" {
				require.Empty(t, response.Jwt, "a user was encoded for a grant naming a system subject")
				require.NotNil(t, refused)
				require.Equal(t, labelSystemSubject, refused.Label)
				return
			}
			require.Nil(t, refused)
			require.NotEmpty(t, response.Jwt)
		})
	}
}

// The deployment's operator identity and the writer hold's probe, as BROKER_NATS_SERVICES lists
// them (config.ParseNATSServices), beside exampleServices.
const (
	operatorAccount = "system:serviceaccount:envoy-operator:envoy-operator"
	probeAccount    = "system:serviceaccount:envoy-operator:envoy-hold-probe"
)

var (
	operatorEntry = config.NATSService{Kind: config.NATSOperator, Account: operatorAccount, AllProjects: true}
	probeEntry    = config.NATSService{Kind: config.NATSProbe, Account: probeAccount, AllProjects: true}
	// holdServices are exampleServices with the operator and the probe listed.
	holdServices = append(append([]config.NATSService{}, exampleServices...), operatorEntry, probeEntry)
)

// The operator entry is answered with natsgrant.Operator of the hold row's phase while the row is
// known, and with released's while it is unknown, so the hold never refuses it; its user expires
// with its token or 10 minutes after the decision, whichever is first, in every phase. Every other
// client a decision admits - a session, a listed service, the hold's probe - is refused
// hold_unknown while the row is unknown, naming nothing but that, and is decided as before while it
// is known, held included (refusing them under held is a later task's). A decision reads the row
// once, and only for a client it would otherwise admit: a client with no credential or an unlisted
// one costs the broker's Postgres nothing.
//
// It fails when the operator is refused for the hold (the unknown row's operator line), when a
// client other than the operator is admitted while the row is unknown, or when the operator's user
// outlives 10 minutes.
func TestTheOperatorsGrantIsTheHoldsPhase(t *testing.T) {
	now := time.Now()
	signer := newHoldSigner(t)
	a := newSession(t, "ses_a", 15*time.Minute)
	tokens := map[string]platformToken{
		"operator-token":       {account: operatorAccount, issued: now, expiry: now.Add(time.Hour)},
		"short-operator-token": {account: operatorAccount, issued: now, expiry: now.Add(3 * time.Minute)},
		"listener-token":       {account: listenerAccount, issued: now, expiry: now.Add(time.Hour)},
		"probe-token":          {account: probeAccount, issued: now, expiry: now.Add(time.Hour)},
		"unlisted-token":       {account: "system:serviceaccount:example-ns:stranger", issued: now, expiry: now.Add(time.Hour)},
	}
	stub := &holdStub{}
	deps := Deps{Introspect: introspector(a), ServiceToken: serviceTokens(tokens), Now: func() time.Time { return now },
		Hold: NewHoldView(stub.read, signer.HoldKeys, nil, nil)}
	operatorName := "operator/" + operatorAccount
	for _, row := range []struct {
		name   string
		answer holdAnswer
		// phase is the operator's grant's phase; known is whether every other client is admitted.
		phase natsgrant.OperatorPhase
		known bool
	}{
		{"released", rowOf(signer.reading(t, 1, natsgrant.OperatorPhaseReleased, "h-1")), natsgrant.OperatorPhaseReleased, true},
		{"prepare", rowOf(signer.reading(t, 2, natsgrant.OperatorPhasePrepare, "h-1")), natsgrant.OperatorPhasePrepare, true},
		{"held", rowOf(signer.reading(t, 3, natsgrant.OperatorPhaseHeld, "h-1")), natsgrant.OperatorPhaseHeld, true},
		{"unknown: no row", holdAnswer{}, natsgrant.OperatorPhaseReleased, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			stub.set(row.answer)
			for _, raw := range []string{"operator-token", "short-operator-token"} {
				before := stub.called()
				decision, err := Decide(t.Context(), deps, holdServices, serviceRequest(raw))
				require.NoError(t, err, "the operator was refused under %s", row.name)
				require.Equal(t, operatorName, decision.Name)
				require.Equal(t, natsgrant.Operator(row.phase), decision.Grant, "%s's grant under %s", raw, row.name)
				require.Equal(t, 1, stub.called()-before, "the operator's decision did not read the row once")
				want := now.Add(10 * time.Minute)
				if raw == "short-operator-token" {
					want = tokens[raw].expiry
				}
				require.Equal(t, want, decision.Expires, "%s's user expires at %s", raw, decision.Expires)
			}
			others := []struct {
				name string
				req  Request
				want Decision
			}{
				{"a session", a.request(t, "nonce"), Decision{Name: "session/ses_a", Grant: must(natsgrant.Session("ses_a")), Expires: time.Unix(a.answer.ExpiresAt, 0)}},
				{"a listener", serviceRequest("listener-token"), Decision{Name: "listener/example-hub", Grant: natsgrant.Listener("example-hub"), Expires: now.Add(time.Hour)}},
				{"the hold probe", serviceRequest("probe-token"), Decision{Name: "probe/" + probeAccount, Grant: natsgrant.HoldProbe(), Expires: now.Add(time.Hour)}},
			}
			for _, other := range others {
				before := stub.called()
				decision, err := Decide(t.Context(), deps, holdServices, other.req)
				require.Equal(t, 1, stub.called()-before, "%s's decision did not read the row once", other.name)
				if row.known {
					require.NoError(t, err, "%s was refused under %s", other.name, row.name)
					require.Equal(t, other.want, decision, "%s under %s", other.name, row.name)
					continue
				}
				var refused *Refusal
				require.True(t, errors.As(err, &refused), "%s was admitted while the hold was unknown: %+v", other.name, decision)
				require.Equal(t, labelHoldUnknown, refused.Label)
				require.Equal(t, "hold_unknown: the NATS writer hold cannot be read", refused.Reason)
				require.ErrorIs(t, refused.Cause, errNoHoldRow, "the reporter's line does not say why")
			}
			for _, refusedFirst := range []Request{serviceRequest("unlisted-token"), {UserNkey: "UUSER", ServerID: "NSERVER"}} {
				before := stub.called()
				_, err := Decide(t.Context(), deps, holdServices, refusedFirst)
				require.Error(t, err)
				require.Zero(t, stub.called()-before, "a client refused before its grant read the hold row")
			}
		})
	}
}

// must is value, failing the calling test's process on err: a grant a literal session id builds.
func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}
