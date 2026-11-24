// Package api is the broker's HTTP surface: routes_table.go lists every route with the
// authentication it needs, and this file authenticates callers and writes responses.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/approval"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/hold"
	"github.com/sjawhar/envoy/internal/cipher/machine"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// DeviceLoginFunc opens a device login's record from the device authorization request's form and
// the user code the provider minted, in the provider's transaction (Deps.DeviceLogin).
type DeviceLoginFunc func(ctx context.Context, form url.Values, userCode string) error

type Deps struct {
	PublicURL string
	// APIAccounts is the Kubernetes half of BROKER_API_ACCOUNTS: the ServiceAccount subject of
	// each listed caller of the routes accountAuth authenticates, answering that caller — its
	// name and the powers its name carries (AccountFor, which is the only way to build one). A
	// subject it does not name is refused, so an account taken off it stops calling as soon as
	// the broker reads it again, and a listed caller reaches only the routes whose power it
	// holds.
	APIAccounts map[string]Account
	// GoogleAPIAccounts is the Google half of the same setting, keyed by the account address a
	// caller's Google identity token carries in its email claim rather than by a subject. It is
	// the Envoy listener on a VM, which holds no Kubernetes token at all; everything else about
	// such a caller — its name, its powers and every refusal — is the Kubernetes caller's.
	GoogleAPIAccounts map[string]Account
	// AccountTokens verifies those callers' projected ServiceAccount tokens, against the same
	// Kubernetes issuer and audience a pod enrolls with (cmd/cipherd passes the one verifier it
	// builds). Nil is a broker that trusts no Kubernetes issuer, where no Kubernetes caller
	// authenticates.
	AccountTokens enroll.PodVerifier
	// GoogleAccountTokens verifies a Google caller's identity token, against the issuer and
	// audience BROKER_GOOGLE_OIDC_ISSUER and BROKER_GOOGLE_OIDC_AUDIENCE name — the audience
	// every machine of the deployment already holds a token for. Nil is a broker with no Google
	// audience, where no Google caller authenticates; with both nil no caller reaches an
	// accountAuth route at all (ACCOUNT_TOKENS_OFF).
	GoogleAccountTokens GoogleVerifier
	Enroll              *enroll.Service
	Machine             *requests.Machine
	MachineLogin        *machine.Service
	Proof               *proof.Verifier
	// PodRegistrars is BROKER_POD_REGISTRARS, the registrar and pod account pairs POST
	// /v1/oauth/exchange/pod-sessions admits: a registration whose caller's login account no
	// entry pairs with the account of the pod it registers is 403 POD_REGISTRAR_NOT_LISTED,
	// whatever BROKER_SERVICE_LOGINS lists. Nil, the setting unset, lists no pair, so the route
	// admits no registration at all.
	PodRegistrars PodRegistrars
	// PodRegistrationTokens verifies a registration's pod token at
	// BROKER_POD_REGISTRATION_AUDIENCE, against the cluster issuer and key set Enroll.Pod verifies
	// the broker's own audience with (cmd/cipherd builds both on one oidc.Keys). Only POST
	// /v1/oauth/exchange/pod-sessions reads it, beside Enroll.Pod, so no other route accepts a
	// token of that audience. Nil is a broker with no such audience, whose registration route
	// accepts the broker's own audience alone.
	PodRegistrationTokens enroll.PodVerifier
	// Policy is the live agent-secret policy, which POST /v1/secrets/{name}/reread rereads one
	// name of (policy.Current.RefreshOne).
	Policy *policy.Current
	// NotifyReread tells every other pod serving this database to reread name, the fan-out
	// internal/cipher/reread's Notifier sends over Postgres LISTEN/NOTIFY; nil means no fan-out,
	// which is a single-pod broker and every test that wires none. POST
	// /v1/secrets/{name}/reread is its one route (notifyPeers, from rereadSecret and from the
	// reread it runs after answering, rereadAfterAnswer), which explains both when it is called
	// and why it is called from the route rather than from RefreshOne. moved is the names whose
	// value this pod's own reread of name may have moved (policy.Lookup.Moved, RefreshOne's), whose
	// held values cmd/cipherd drops from its airlock proxy.
	//
	// It reports nothing, because the handler has nothing to do with a failure: the local reread
	// has already succeeded and its answer stands, so whether a failed fan-out is logged, and at
	// what level, belongs to the function cmd/cipherd supplies.
	NotifyReread func(ctx context.Context, name string, moved []string)
	// Detached counts the work a route starts that outlives its answer, the reread run after a
	// 202, for the stop sequence to wait on (Detached.Wait); nil counts nothing, for a mux nothing
	// stops.
	Detached *Detached
	// SecretsPrefix is BROKER_SECRETS_PREFIX, the namespace GET /v1/settings answers.
	SecretsPrefix string
	// SecretsStore is BROKER_SECRETS_STORE, the store GET /v1/settings names: aws.Name or
	// gcp.Name.
	SecretsStore string
	// SecretsKMSKey is the agent-secrets key GET /v1/settings answers: the AWS store's key ARN
	// (BROKER_SECRETS_KMS_KEY_ARN), with its region and account, or the GCP store's Cloud KMS key
	// name (BROKER_SECRETS_KMS_KEY_NAME).
	SecretsKMSKey string
	// GoogleExchangeAudience is BROKER_GOOGLE_OIDC_AUDIENCE where BROKER_VM_SERVICES lists a
	// machine that may trade a Google identity token at the token exchange, and empty otherwise.
	// GET /v1/settings answers it, so a machine's `agent-secrets` mints a token for the right
	// audience with no setting of its own.
	GoogleExchangeAudience string
	// LauncherLimits bounds POST /v1/oauth/device_authorization and POST
	// /v1/launcher-credentials/token; nil means DefaultLauncherLimits.
	LauncherLimits *LauncherLimits
	// RereadLimit bounds POST /v1/secrets/{name}/reread per source address; nil means
	// RereadLimitFor of RereadOverallLimit, so that an address's own rate is ordered against
	// the bucket this process really mounts rather than against a constant derived from a
	// bucket it may never mount.
	RereadLimit *ratelimit.Limit
	// RereadOverallLimit bounds that route across every caller at once; nil means
	// DefaultRereadOverallLimit.
	RereadOverallLimit *ratelimit.Limit
	// RereadRate derives both reread buckets from the replica count this process counts and
	// moves them whenever that count changes. The route is served from that rate's own pair
	// whenever it is set (RereadRate.Buckets), so RereadLimit and RereadOverallLimit are then
	// read by nothing. Only cmd/cipherd sets it, from the fleet budget of the store it mounts,
	// and only its serving clock changes the count.
	//
	// nil is every rate fixed at registration, which is what a test gets: cmd/cipherd always
	// builds one, so no broker a deployment runs is without it. A test that wants rates that
	// never move passes none and pins the two limits above, or takes their defaults.
	RereadRate *RereadRate
	// TokenLimit bounds POST /v1/oauth/token per source address; nil means DefaultTokenLimit.
	TokenLimit *ratelimit.Limit
	// AccountLimit bounds every route accountAuth authenticates per source address, in one bucket
	// the whole class shares and spent before the caller's token is verified, and with it the
	// approval page's routes and the sign-in's (authenticate, signInStatus); nil means
	// DefaultAccountLimit.
	AccountLimit *ratelimit.Limit
	// TrustedProxyHeader names a request header (e.g. "X-Forwarded-For") the rate limiters'
	// per-address buckets trust for the real client address; empty means keying on r.RemoteAddr,
	// correct only when the broker is reached directly rather than through a reverse proxy or load
	// balancer. See limits.go's clientAddress.
	TrustedProxyHeader string
	// OAuth is the machine login's OAuth provider and the token exchange. POST
	// /v1/oauth/device_authorization, POST /v1/oauth/token, POST /v1/oauth/introspect, POST
	// /v1/oauth/revoke, POST /v1/oauth/exchange/pod-sessions and POST
	// /v1/oauth/exchange/pod-sessions/release are mounted only when it is set, with DeviceLogin
	// and both TokenHooks, which it then requires (Register panics without them).
	OAuth *oauth.Provider
	// DeviceLogin opens a device login's record from the device authorization request's form and
	// the user code the provider minted, in the provider's transaction (oauthstore.TxFrom on ctx);
	// an error refuses the request (deviceLoginRefusal maps it to the broker's code).
	DeviceLogin DeviceLoginFunc
	// TokenHooks are the broker's steps around each token the provider issues.
	TokenHooks oauth.TokenHooks
	// Approval is the broker's own approval page and its sign-in (internal/cipher/approval),
	// which cmd/cipherd builds from BROKER_SIGNIN_* unless BROKER_SIGNIN=off, when it builds none.
	// Its routes, every one under /approve and /signin/, are mounted only when it is set, so a
	// broker that signs nobody in, and a test rig that wires no page, answer each of them 404.
	Approval *approval.Page
	// Hold is the check of whether Dispatch holds a person's credentials until they sign in
	// again (hold.Gate.Decide), which cmd/cipherd builds where BROKER_PROJECT_ACCESS_URL is set, on
	// the Dispatch client the token exchange asks. Nil is a broker with no Dispatch to ask, which
	// holds nobody.
	Hold *hold.Gate
	// Draining reports whether this process has begun stopping, which GET /healthz answers 503
	// SHUTTING_DOWN for while every other route goes on being served; nil means it never does.
	//
	// This is how a pod stops being sent traffic before it stops accepting it. Kubernetes removes
	// a terminating pod from its Service's EndpointSlice concurrently with the kubelet beginning
	// termination, not before it, so a pod that simply closed its listener at SIGTERM would be
	// refusing connections that were still being routed to it. Failing the health check is what
	// makes a readiness probe, and anything else reading readiness, stop choosing this pod, which
	// is why the drain has to show up here rather than in the listener. nil is every caller that
	// never stops in the first place - ciphertest and the in-process rigs - for which the broker
	// is never draining.
	Draining func() bool
	// ProxyReady reports whether the proxy's certificate authority can sign a bound host's leaf
	// (proxy.Authority.Ready): nil while it holds a certificate whose life is not over. While it
	// errs GET /healthz answers 503 NO_INTERMEDIATE with its reason, so a readiness probe keeps
	// traffic away from a pod whose every bound handshake would fail, and a rolling update keeps
	// the old pods until a new one has its intermediate. This process goes on serving every other
	// route, but that holds only inside it: Kubernetes takes a pod that fails its readiness probe
	// out of every Service that selects it, and the edge's GET /healthz answers from these same
	// Deps, so an unready pod serves no route to anyone. Nil is a broker with no proxy, and every
	// rig that wires none.
	ProxyReady func() error
	// NATSHold is the NATS writer hold's routes, /v1/nats-hold and the three under it, which
	// the operator alone calls (NATSHold). Every Deps mounts them; the zero NATSHold refuses
	// every caller 403 not_operator, as a broker whose BROKER_NATS_SERVICES lists no operator
	// does.
	NATSHold NATSHold
	// DatabasePing is the Postgres ping GET /healthz answers from; nil means Enroll.Store.Pool.Ping.
	// RegisterEdge sets the edge's to a one-second cache over it (cachedPing).
	DatabasePing func(context.Context) error
	// EdgeListeners is each edge listener's entry of BROKER_NATS_SERVICES
	// (config.NATSCallout.EdgeListeners): GET /v1/project-repositories answers the account an entry
	// names, presenting its own identity token as a listed caller does, about the projects that
	// entry lists and no others, and refuses every other account. cmd/cipherd reads them whether or
	// not it runs the NATS auth service. Nil lists none, where that route answers no caller.
	EdgeListeners []EdgeListener
	// ProjectRepositories reads Dispatch's answer for GET /v1/project-repositories, fresh on
	// every call (projectrepos.Client, at BROKER_PROJECT_ACCESS_URL with the account token
	// projectaccess presents). Nil is a broker that reads no Dispatch, where that route answers
	// every listed edge listener 503 REPOSITORIES_UNKNOWN.
	ProjectRepositories ProjectRepositoriesReader
}

// pingDatabase pings Postgres: DatabasePing when set, and the store's pool otherwise.
func (d Deps) pingDatabase(ctx context.Context) error {
	if d.DatabasePing != nil {
		return d.DatabasePing(ctx)
	}
	return d.Enroll.Store.Pool.Ping(ctx)
}

type server struct {
	deps            Deps
	launcherLimiter *launcherLimiter
	// rereadLimiter is the per-address bucket on POST /v1/secrets/{name}/reread
	// (address) and rereadOverall the one every caller shares; refuseReread takes from
	// both or neither. For a Deps that carries a RereadRate these are that rate's own pair, so
	// a change of the replica count moves the rates this route really runs at.
	rereadLimiter *ratelimit.Keyed
	rereadOverall *ratelimit.Bucket
	// tokenLimiter is TokenLimit applied per source address (address), over its route's own
	// bucket, and accountLimiter AccountLimit, over the bucket every accountAuth route and every
	// route of the approval page share.
	tokenLimiter   *ratelimit.Keyed
	accountLimiter *ratelimit.Keyed
	// holdFloors is this process's floor of each NATS writer hold record (recordFloors), kept
	// for as long as the routes Register mounted serve. holdRowFloor is its floor of the hold row
	// (NATSHold.RowFloor, which the auth service's hold view shares): the highest verified row
	// either has served, read or written, with that row's digest. A database writer other than the
	// operator cannot write a row that verifies, but can put back an older one the operator signed
	// (a prepare row put back mid-hold would lead the operator to sign a release from it), or one
	// of two rows signed at one gen. So GET serves no verified row the floor reads as a replay or a
	// fork (409 below_floor, which the operator's client reads as unknown), a move from a replayed
	// row is 409 gen_mismatch and from a forked one 409 below_floor, and a restore at or below the
	// floor is 409 below_floor. The residual, the records floor's too: each replica keeps its own,
	// for as long as its process lives, so a replica that starts again, or one that has not served,
	// read or written the row since its newer gen, serves an older row put back as it would the
	// genuine one.
	holdFloors   *recordFloors
	holdRowFloor *signedrecord.Floor
	// address is the source address every per-address bucket of this listener keys on:
	// clientAddress under BROKER_TRUSTED_PROXY_HEADER on the broker's own address, and
	// edgeClientAddress at its edge, where a request it reads no address from is refused before
	// any handler runs (edgeFormGate).
	address func(*http.Request) string
	// loginKeyPrefix opens every per-login bucket's key: "" on the broker's own address, and
	// edgeLoginKeyPrefix at its edge.
	loginKeyPrefix string
	// loginKeyByAddress is whether a person's per-login bucket is also keyed on the caller's
	// address (spendDeviceLoginBucket): at the edge, where anyone on the internet can name a
	// person, so a flood from one address cannot empty the bucket that person uses from another.
	loginKeyByAddress bool
	// verificationURI is the page a device authorization on this listener sends a person to:
	// BROKER_MACHINE_LOGIN_URL on the broker's own address (oauth.Provider.VerificationURL), and
	// BROKER_EDGE_MACHINE_LOGIN_URL at its edge.
	verificationURI string
	// pingLogsItself is whether deps.DatabasePing writes its own line for a failed ping, so GET
	// /healthz writes none: at the edge, whose DatabasePing is a cachedPing, so a caller on the
	// internet writes no line of its own (RegisterEdge sets the two together).
	pingLogsItself bool
	// repositoryCalls counts each edge listener's calls of GET /v1/project-repositories still
	// waiting on Dispatch, at most maxRepositoryCallsInFlight each.
	repositoryCalls *callsInFlight
	// ownOnlyLogged lets MachineLoginsOwnOnlyMessage be written once per person per
	// ownOnlyLogEvery (machineLoginsOwnOnly).
	ownOnlyLogged *ratelimit.Keyed
}

func Register(mux *http.ServeMux, deps Deps) {
	if deps.OAuth != nil && (deps.MachineLogin == nil || deps.DeviceLogin == nil || deps.TokenHooks.Before == nil || deps.TokenHooks.After == nil) {
		panic("api: Deps.OAuth needs MachineLogin, DeviceLogin and both TokenHooks")
	}
	if deps.NATSHold.OperatorToken != nil && (deps.NATSHold.Pool == nil || deps.NATSHold.Schema == "") {
		panic("api: Deps.NATSHold.OperatorToken needs the Pool and the Schema of the hold's tables")
	}
	limits := DefaultLauncherLimits
	if deps.LauncherLimits != nil {
		limits = *deps.LauncherLimits
	}
	// The two buckets POST /v1/secrets/{name}/reread is served from. A Deps that carries a rate
	// is served from the rate's own pair, which it moves at each count the serving clock gives;
	// one that does not gets a fixed pair, at the limits it pinned or their defaults.
	var rereadLimiter *ratelimit.Keyed
	var rereadBucket *ratelimit.Bucket
	if deps.RereadRate != nil {
		rereadLimiter, rereadBucket = deps.RereadRate.Buckets()
	} else {
		rereadOverall := DefaultRereadOverallLimit
		if deps.RereadOverallLimit != nil {
			rereadOverall = *deps.RereadOverallLimit
		}
		// Derived from the shared bucket resolved just above, and only when the caller named no
		// bucket of its own: a caller that passes one - the rows that drive the per-address
		// bucket on its own - keeps what it passed, exactly as every other optional dep here is
		// defaulted rather than overridden.
		reread := RereadLimitFor(rereadOverall)
		if deps.RereadLimit != nil {
			reread = *deps.RereadLimit
		}
		rereadLimiter, rereadBucket = ratelimit.NewKeyed(reread), ratelimit.NewBucket(rereadOverall)
	}
	token := DefaultTokenLimit
	if deps.TokenLimit != nil {
		token = *deps.TokenLimit
	}
	account := DefaultAccountLimit
	if deps.AccountLimit != nil {
		account = *deps.AccountLimit
	}
	address := func(r *http.Request) string { return clientAddress(r, deps.TrustedProxyHeader) }
	s := &server{
		deps:            deps,
		launcherLimiter: newLauncherLimiter(limits, address),
		rereadLimiter:   rereadLimiter,
		rereadOverall:   rereadBucket,
		tokenLimiter:    ratelimit.NewKeyed(token),
		accountLimiter:  ratelimit.NewKeyed(account),
		holdFloors:      newRecordFloors(),
		holdRowFloor:    deps.NATSHold.RowFloor,
		address:         address,
		repositoryCalls: newCallsInFlight(maxRepositoryCallsInFlight),
		ownOnlyLogged:   ratelimit.NewKeyed(ratelimit.Limit{Every: ownOnlyLogEvery, Burst: 1}),
	}
	if s.holdRowFloor == nil {
		s.holdRowFloor = &signedrecord.Floor{}
	}
	if deps.OAuth != nil {
		s.verificationURI = deps.OAuth.VerificationURL()
	}
	for _, route := range routes() {
		// A broker with no provider serves neither the OAuth routes nor the document that names
		// them, and one whose provider composes no device grant (no BROKER_MACHINE_LOGIN_URL,
		// so no page for a person to type a code into) serves no device authorization route:
		// the row is simply not mounted, so the path is a 404 rather than a route answering an
		// error no client could act on.
		if deps.OAuth == nil && (strings.HasPrefix(route.Pattern, oauthRoutes) || route.Pattern == metadataPath) {
			continue
		}
		if deps.OAuth != nil && !deps.OAuth.DeviceGrant() && route.Pattern == deviceAuthorizationPath {
			continue
		}
		if deps.Approval == nil && (strings.HasPrefix(route.Pattern, approvalRoutes) || strings.HasPrefix(route.Pattern, signInRoutes)) {
			continue
		}
		mux.HandleFunc(route.Method+" "+route.Pattern, s.handle(route))
	}
}

// handle is route as a handler of s's listener: the caller authenticated as the row's class
// requires, the answer's record told who it is, the row's hold step run (routeHandler.hold), and
// the row's handler run. Register mounts every row through it, and RegisterEdge the edge's three
// behind its gates, so a row runs one way on either listener.
func (s *server) handle(route apiRoute) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recorder := answer.FromContext(r.Context())
		who, ok := s.authenticate(w, r, route)
		if !ok {
			// A refusal raised in authentication names no caller, none having been established.
			recorder.NoteCaller(answer.CallerUnauthenticated, "")
			return
		}
		// The caller is named before the hold step, so its refusal, and the reads it makes, name
		// the caller the credential proved.
		recorder.NoteCaller(who.answerCaller(route.Handler))
		if route.Handler.hold != nil && route.Handler.hold(s, w, r, who) {
			return
		}
		route.Handler.serve(s, w, r, who)
	}
}

// oauthRoutes prefixes the routes the OAuth provider serves, which Register mounts only with one.
const oauthRoutes = "/v1/oauth/"

// approvalRoutes and signInRoutes prefix the approval page's routes, which Register mounts only
// with the page: a broker with BROKER_SIGNIN=off has none, nor does a rig that wires none, and
// either serves those paths not at all.
const (
	approvalRoutes = "/approve"
	signInRoutes   = "/signin/"
)

// authenticate proves who r comes from, as route's authentication requires, or writes the
// refusal. A bad or missing credential is a 401; a store that cannot answer is a 503 naming it,
// never mistaken for a bad credential. A route a ServiceAccount token authenticates — one a
// listed caller's token does (accountAuth), one a service's own login token does (serviceAuth)
// and one an edge listener's token does (edgeListenerAuth) — first spends its source address's
// bucket (DefaultAccountLimit), a refusal being 429 RATE_LIMITED with Retry-After, so a flood of
// forged tokens past that bucket costs no verification and no fetch of the issuer's keys; a
// listed caller must then hold the power its row declares, a service's token must name an
// account BROKER_SERVICE_LOGINS lists and BROKER_POD_OPERATORS does not map (serviceCaller), and
// an edge listener's must name an account an edge-listener entry of BROKER_NATS_SERVICES names
// (edgeListenerCaller). The NATS writer hold's routes (operatorAuth) spend that bucket before the
// operator's token is verified, for the same reason, and the approval page's routes spend it
// too: see the authSignIn case.
// A session route and a launcher route ask the hold gate after authenticate, as their own step in
// handle (routeHandler.hold).
func (s *server) authenticate(w http.ResponseWriter, r *http.Request, route apiRoute) (caller, bool) {
	auth := route.Handler.auth
	switch auth {
	case authNone:
		return caller{}, true
	case authAccount:
		if s.refuseAccountAddress(w, r) {
			return caller{}, false
		}
		who, ok := s.accountCaller(w, r)
		if !ok {
			return caller{}, false
		}
		// Being listed is not being allowed: the caller's name answers which powers it holds and
		// this row declares the one it needs, so a caller listed for introspection alone never
		// relays a person's decision. ACCOUNT_NOT_LISTED and this are different facts — an
		// account the broker does not know at all, and a known caller reaching past its own
		// routes — so each has its own code. The refusal names the caller and the route, and
		// nothing of the token presented.
		if refuseWithoutPower(w, who.account, route.Handler.power, route.Method+" "+route.Pattern) {
			return caller{}, false
		}
		return who, true
	case authService:
		if s.refuseAccountAddress(w, r) {
			return caller{}, false
		}
		return s.serviceCaller(w, r)
	case authOperator:
		if s.refuseAccountAddress(w, r) {
			return caller{}, false
		}
		return s.operatorCaller(w, r)
	case authEdgeListener:
		if s.refuseAccountAddress(w, r) {
			return caller{}, false
		}
		return s.edgeListenerCaller(w, r)
	case authSignIn:
		// The page's routes and the sign-in's spend the bucket the account class spends, so no
		// caller drives them unmetered either. GET /approve/{record} with no session does one
		// record read and hands the caller a valid __Host-broker-signin cookie; GET
		// /signin/callback with that cookie's own state makes the broker send a token request to
		// its sign-in provider, with the broker's client credentials, and write the failed
		// sign-in row; and POST /approve and POST /signin/logout each read and write a session's
		// row. GET /signin/status spends it in its own handler, being public, and the page's
		// stylesheet spends nothing: it is bytes of the binary, and every page is read under it.
		// The spend comes before the page reads anything, and its refusal is the page's own
		// 429, under the page's headers, since a browser is told nothing about a ServiceAccount
		// token: only the Retry-After is the one refuseAccountAddress would have written.
		if !s.accountAddressAllows(r) {
			s.deps.Approval.WriteRateLimited(w, ratelimit.RetryAfterSeconds(s.accountLimiter.Every()))
			return caller{}, false
		}
		session, ok := s.deps.Approval.Authenticate(w, r)
		return caller{signIn: session}, ok
	case authLauncher:
		return s.launcherCaller(w, r)
	case authProof:
		return s.sessionCaller(w, r, "PROOF_INVALID", "proof invalid: not live")
	case authRenewProof:
		return s.sessionCaller(w, r, "ENROLLMENT_NOT_LIVE", "enrollment is not live")
	}
	writeInternal(w, "authenticate", fmt.Errorf("route has unknown authentication %d", auth))
	return caller{}, false
}

// answerCaller is the caller class and identity the answer's record names for a request
// authenticate let through on route (answer.Recorder.NoteCaller): the launcher credential, the
// enrollment, the listed account, the service and the account it presented, the writer hold's
// operator, as the service operator@<its account>, the edge listener's account, or the signed-in
// person. A route that takes no credential is public, and a route of the approval page reached
// with no live sign-in unauthenticated; a handler that authenticates its own caller notes its
// class over this one (tokenLogin, and rereadSecret for a session's proof).
func (c caller) answerCaller(route routeHandler) (class, id string) {
	switch route.auth {
	case authLauncher:
		return answer.CallerLauncher, c.launcher.ID.String()
	case authProof, authRenewProof:
		return answer.CallerSession, c.enrollment
	case authAccount:
		return answer.CallerAccount, c.account.Name
	case authService:
		return answer.CallerService, serviceIdentity(c.service)
	case authOperator:
		return answer.CallerService, "operator@" + c.operator
	case authEdgeListener:
		return answer.CallerEdgeListener, c.edge.Account
	case authSignIn:
		if c.signIn.Login == "" {
			return answer.CallerUnauthenticated, ""
		}
		return answer.CallerSignIn, c.signIn.Login
	}
	return answer.CallerPublic, ""
}

// serviceIdentity is a service's identity in the answer's record: the service it logs in as and
// the account whose token it presented, <service>@<namespace>/<name>.
func serviceIdentity(account machine.ServiceAccount) string {
	return account.Service + "@" + account.Host
}

// refuseWithoutPower writes 403 ACCOUNT_LACKS_POWER and reports true when the listed caller who
// does not hold needs, the power route declares ("POST /v1/oauth/revoke", the method and the
// pattern the broker mounted). authenticate calls it for every accountAuth row before the
// handler runs.
func refuseWithoutPower(w http.ResponseWriter, who Account, needs power, route string) bool {
	if who.may(needs) {
		return false
	}
	writeError(w, http.StatusForbidden, "ACCOUNT_LACKS_POWER", fmt.Sprintf("the caller %q may not call %s", who.Name, route))
	return true
}

// launcherCaller authenticates a launcher proof for authenticate, answering the machine login's
// credential the proof's key holds: what launcherAuth's and launcherEndAuth's routes take.
func (s *server) launcherCaller(w http.ResponseWriter, r *http.Request) (caller, bool) {
	subject, err := s.proofSubject(r)
	switch {
	case errors.Is(err, proof.ErrInvalid), errors.Is(err, proof.ErrNotLive):
		writeError(w, http.StatusUnauthorized, "LAUNCHER_INVALID", "the launcher credential is not valid")
		return caller{}, false
	case err != nil:
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "verify launcher proof", err)
		return caller{}, false
	}
	if subject.LauncherID == "" {
		writeError(w, http.StatusUnauthorized, "LAUNCHER_INVALID", "this route needs a launcher proof, not a session proof")
		return caller{}, false
	}
	// By the key the proof proved and not by the launcher id alone: a login writes its own key
	// and pod uid onto the credential row (enroll.Service.IssueTokenLoginTx), so a login landing
	// between the proof's own lookup and this read moves the row out from under the proof, and a
	// read by id alone would hand the handler whatever identity the row then holds. A login bound
	// to no pod takes one row per subject on either schema, and a pod's own re-login moves its own
	// row under the same id, so the pair is right on either (enroll.Service.CredentialForKey).
	cred, err := s.deps.Enroll.CredentialForKey(r.Context(), subject.LauncherID, subject.Thumbprint)
	switch {
	case errors.Is(err, enroll.ErrNoCredential):
		// The key that proved this request holds the row no longer, so it authenticates nothing —
		// the same answer this call site gives every credential that is not valid, rather than the
		// 503 of a store that cannot answer.
		writeError(w, http.StatusUnauthorized, "LAUNCHER_INVALID", "the launcher credential is not valid")
		return caller{}, false
	case err != nil:
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "read launcher credential", err)
		return caller{}, false
	}
	return caller{launcher: cred}, true
}

// sessionCaller authenticates a session proof, for authenticate on every session route and for
// the reread route's handler on a call that carries a Proof header (rereadSecret), answering a
// proof whose enrollment is no longer live with notLiveCode and notLiveMessage: PROOF_INVALID on
// every session route but renew, which alone answers ENROLLMENT_NOT_LIVE so a pod renewer can
// stop. Every other bad proof is PROOF_INVALID on both.
func (s *server) sessionCaller(w http.ResponseWriter, r *http.Request, notLiveCode, notLiveMessage string) (caller, bool) {
	subject, err := s.proofSubject(r)
	switch {
	case errors.Is(err, proof.ErrNotLive):
		writeError(w, http.StatusUnauthorized, notLiveCode, notLiveMessage)
		return caller{}, false
	case errors.Is(err, proof.ErrInvalid):
		writeError(w, http.StatusUnauthorized, "PROOF_INVALID", err.Error())
		return caller{}, false
	case err != nil:
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "verify proof", err)
		return caller{}, false
	}
	if subject.EnrollmentID == "" {
		writeError(w, http.StatusUnauthorized, "PROOF_INVALID", "this route needs a session proof, not a launcher proof")
		return caller{}, false
	}
	return caller{enrollment: subject.EnrollmentID}, true
}

// refuseHeldLauncher is launcherAuth's hold step: refuseHeld of the person the machine login's
// credential names as its operator, none for a service's login.
func (s *server) refuseHeldLauncher(w http.ResponseWriter, r *http.Request, who caller) bool {
	return s.refuseHeld(w, r, derefOr(who.launcher.Operator, ""))
}

// refuseHeldProof is sessionAuth's hold step: refuseHeldSession of the session's enrollment.
func (s *server) refuseHeldProof(w http.ResponseWriter, r *http.Request, who caller) bool {
	return s.refuseHeldSession(w, r, who.enrollment)
}

// refuseHeldSession is refuseHeld for a session proof, whose proof row names no operator: it reads
// the enrollment's own (enroll.Service.EnrollmentOperator), and only when the broker has a gate to
// ask. An enrollment gone since its proof verified is refused as a proof that is not live. A pod's
// enrollment names the person BROKER_POD_OPERATORS mapped its account to when its login was made,
// and is asked about that person, as a person's session is; any other pod's names none, and is
// asked nothing. sessionAuth's hold step calls it on every session route but those that only end
// access or renew, and rereadSecret on a reread a session signs.
func (s *server) refuseHeldSession(w http.ResponseWriter, r *http.Request, enrollmentID string) bool {
	if s.deps.Hold == nil {
		return false
	}
	operator, err := s.deps.Enroll.EnrollmentOperator(r.Context(), enrollmentID)
	switch {
	case errors.Is(err, enroll.ErrNoEnrollment):
		writeError(w, http.StatusUnauthorized, "PROOF_INVALID", "proof invalid: not live")
		return true
	case err != nil:
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "read the enrollment's operator", err)
		return true
	}
	return s.refuseHeld(w, r, operator)
}

// refuseHeld asks the hold gate (Deps.Hold) whether Dispatch holds person, the operator a
// session's enrollment or a machine login's credential names, and writes the refusal when it does
// or cannot tell, reporting whether it refused, by the gate's Verdict: 403 MEMBERSHIP_HELD naming
// the person in the gate's words for hold.Held, and 503 HOLD_UNCHECKABLE, saying why in fixed
// words, for every kind that decides nothing, a held answer the gate could not keep among them
// (hold.Verdict.Words). The gate's own error goes to the log alone. No person (an unmapped pod's
// enrollment, a service's login) is asked nothing, and neither is anyone on a broker with no
// gate. The codes are written here, not taken from hold.Verdict.Refusal, because broker-refgen
// reads each refusal's code from writeError's own argument.
func (s *server) refuseHeld(w http.ResponseWriter, r *http.Request, person string) bool {
	if person == "" {
		return false
	}
	switch v := s.deps.Hold.Decide(r.Context(), person); v.Kind {
	case hold.Served:
		return false
	case hold.Held:
		writeError(w, http.StatusForbidden, "MEMBERSHIP_HELD", v.Words())
	default:
		slog.Error("broker: whether Dispatch holds a person cannot be read, so their credential is refused", "person", person, "error", v.Err)
		writeError(w, http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", v.Words())
	}
	return true
}

// GoogleVerifier verifies a caller's own Google identity token and reads the identity claims of
// the verified token (GoogleTokens, the broker's one reading of Google's claim shapes). It is an
// interface here so a test can wire a verifier of its own, as the Kubernetes one is.
type GoogleVerifier interface {
	VerifyGoogle(ctx context.Context, raw string) (GoogleToken, error)
}

// accountCaller authenticates a listed caller's own identity token for authenticate, the
// credential accountAuth's routes take, and answers the caller BROKER_API_ACCOUNTS names it:
// its name and its powers, which authenticate then checks against the route's. The token is
// verified as identityAccount verifies it; a sound one naming an account BROKER_API_ACCOUNTS does
// not name is 403 ACCOUNT_NOT_LISTED, the one refusal that is the broker's list and not the
// credential.
func (s *server) accountCaller(w http.ResponseWriter, r *http.Request) (caller, bool) {
	account, google, ok := s.identityAccount(w, r)
	if !ok {
		return caller{}, false
	}
	accounts := s.deps.APIAccounts
	if google {
		accounts = s.deps.GoogleAPIAccounts
	}
	listed, found := accounts[account]
	if !found {
		writeError(w, http.StatusForbidden, "ACCOUNT_NOT_LISTED", "this account is not in BROKER_API_ACCOUNTS")
		return caller{}, false
	}
	return caller{account: listed}, true
}

// edgeListenerCaller authenticates an edge listener's own identity token for authenticate, the
// credential edgeListenerAuth's routes take, and answers the edge-listener entry of
// BROKER_NATS_SERVICES that names its account (Deps.EdgeListeners). The token is the one a listed
// caller presents, verified the same way (identityAccount); a sound one naming an account no
// edge-listener entry names is 403 NOT_AN_EDGE_LISTENER, whatever else lists that account: the
// Envoy listener's entry of kind listener, or its BROKER_API_ACCOUNTS name, makes it no edge
// listener.
func (s *server) edgeListenerCaller(w http.ResponseWriter, r *http.Request) (caller, bool) {
	account, google, ok := s.identityAccount(w, r)
	if !ok {
		return caller{}, false
	}
	for _, edge := range s.deps.EdgeListeners {
		if edge.Account == account && edge.Google == google {
			return caller{edge: edge}, true
		}
	}
	writeError(w, http.StatusForbidden, "NOT_AN_EDGE_LISTENER", "this account is named by no edge-listener entry of BROKER_NATS_SERVICES")
	return caller{}, false
}

// identityAccount verifies r's bearer, a caller's own identity token, and answers the account it
// names and whether that is a Google account, or writes the refusal: the one verification
// accountCaller and edgeListenerCaller share.
//
// A caller presents one of two tokens: a Kubernetes service account's own projected token, which
// names the account by its subject, or a machine's Google identity token, which names it by its
// email claim, which is how the Envoy listener calls from a VM that holds no Kubernetes token.
// Which verifier judges a token is the token's own issuer: a verifier answers oidc.ErrIssuer for
// a token of any issuer but its own, which is the one error that means "not mine" rather than a
// refusal, so a token neither verifier claims is simply invalid and no token is ever judged
// twice.
//
// The taxonomy is the one a daemon's token login uses (machine.Service.VerifyServiceToken) and
// means the same thing for either kind: a broker that trusts no issuer for its callers at all
// (ACCOUNT_TOKENS_OFF), a token that has expired, one that does not verify — which for a Google
// token includes one naming no verified account, since the email claim is what names the caller
// — and one that lives longer than machine.MaxTokenLoginLifetime from its issue, so a copied
// token is spendable for at most an hour. Each is a 401, and no refusal repeats the token.
func (s *server) identityAccount(w http.ResponseWriter, r *http.Request) (account string, google, ok bool) {
	if s.deps.AccountTokens == nil && s.deps.GoogleAccountTokens == nil {
		writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKENS_OFF", "this broker trusts no token issuer for its API callers, so no caller authenticates with an identity token")
		return "", false, false
	}
	raw := bearer(r)
	if s.deps.AccountTokens != nil {
		claims, err := s.deps.AccountTokens.VerifyAccount(r.Context(), raw)
		if !errors.Is(err, oidc.ErrIssuer) {
			return claims.Subject, false, s.liveIdentityToken(w, err, claims.IssuedAt, claims.Expiry)
		}
	}
	if s.deps.GoogleAccountTokens != nil {
		token, err := s.deps.GoogleAccountTokens.VerifyGoogle(r.Context(), raw)
		if !errors.Is(err, oidc.ErrIssuer) {
			if err == nil && (token.Identity.Account == "" || !token.Identity.EmailVerified) {
				writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID", "the Google identity token names no verified account, so it names no caller (ask for it with format=full)")
				return "", false, false
			}
			return token.Identity.Account, true, s.liveIdentityToken(w, err, token.IssuedAt, token.Expiry)
		}
	}
	writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID", "the caller's token names no issuer this broker trusts for its API callers")
	return "", false, false
}

// liveIdentityToken is identityAccount's judgement of one token its verifier judged, whichever
// kind it is: err is what the verifier answered. It reports whether the token is sound, or
// writes the refusal.
func (s *server) liveIdentityToken(w http.ResponseWriter, err error, issuedAt, expiry time.Time) bool {
	switch {
	case errors.Is(err, oidc.ErrExpired):
		writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKEN_EXPIRED", "the caller's identity token has expired")
		return false
	case err != nil:
		writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID", "the caller's identity token does not verify")
		return false
	case issuedAt.IsZero():
		writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKEN_TOO_LONG_LIVED", "the caller's identity token names no issue time (iat)")
		return false
	case expiry.Sub(issuedAt) > machine.MaxTokenLoginLifetime+s.deps.Proof.Skew:
		writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKEN_TOO_LONG_LIVED", "the caller's identity token lives longer than an hour from its issue")
		return false
	}
	return true
}

// serviceCaller authenticates a service's own projected ServiceAccount token for authenticate,
// the credential serviceAuth's routes take, and answers the login that token made as one
// machine.ServiceAccount, which serviceAuth hands the handler. It is the token login's own
// check (machine.Service.VerifyServiceToken), so a route of this class and a daemon's token
// login accept exactly the same credential: the same verifier, the same hour-from-issue bound
// on a copied token, and the same list. An account the list does not name is 403
// ACCOUNT_NOT_LISTED, the credential being sound and the broker's list refusing; a broker that
// trusts no Kubernetes token issuer answers 401 ACCOUNT_TOKENS_OFF, as this class's routes then
// authenticate no one. No refusal repeats the token.
//
// A verified account that BROKER_POD_OPERATORS maps to a person (machine.ServiceAccount.Person)
// is 403 PERSON_ACCOUNT, and the refusal names the account. A mapped account is a person's
// account, and it is never a service's caller. So a person's pod calls no route of this class.
// The refusal is the same for a token bound to a pod and for a token bound to no pod.
func (s *server) serviceCaller(w http.ResponseWriter, r *http.Request) (caller, bool) {
	account, err := s.deps.MachineLogin.VerifyServiceToken(r.Context(), bearer(r))
	switch {
	case errors.Is(err, machine.ErrTokenLoginsOff):
		writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKENS_OFF", "this broker trusts no Kubernetes token issuer, so no service authenticates with a ServiceAccount token")
		return caller{}, false
	case errors.Is(err, machine.ErrTokenExpired):
		writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKEN_EXPIRED", "the ServiceAccount token has expired")
		return caller{}, false
	case errors.Is(err, machine.ErrTokenTooLongLived):
		writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKEN_TOO_LONG_LIVED", "the ServiceAccount token lives longer than an hour from its issue")
		return caller{}, false
	case errors.Is(err, machine.ErrAccountNotListed):
		writeError(w, http.StatusForbidden, "ACCOUNT_NOT_LISTED", "this ServiceAccount is not in BROKER_SERVICE_LOGINS")
		return caller{}, false
	case err != nil:
		writeError(w, http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID", "the ServiceAccount token does not verify")
		return caller{}, false
	}
	if account.Person != "" {
		writeError(w, http.StatusForbidden, "PERSON_ACCOUNT",
			fmt.Sprintf("BROKER_POD_OPERATORS maps the ServiceAccount %s to a person, and a person's account calls no service route", account.Subject))
		return caller{}, false
	}
	return caller{service: account}, true
}

// proofSubject verifies r's Proof header and returns who it authenticates, unwrapped: authLauncher
// and authProof each translate a failure into their own vocabulary (LAUNCHER_INVALID vs
// PROOF_INVALID), so this reports
// only the raw error.
func (s *server) proofSubject(r *http.Request) (proof.Subject, error) {
	return s.deps.Proof.Verify(r.Context(), r.Header.Get("Proof"), r.Method, s.deps.PublicURL+r.URL.Path, time.Now())
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// maxJSONBodyBytes caps every JSON request body the broker reads, as Dispatch's own decodeJSON does.
const maxJSONBodyBytes = 1 << 20

// readJSON decodes r's body as exactly one JSON value into v: at most maxJSONBodyBytes (413
// REQUEST_TOO_LARGE), no field v does not declare and nothing after the value (400 with
// invalidCode). It writes the refusal itself and reports whether decoding succeeded.
func readJSON(w http.ResponseWriter, r *http.Request, v any, invalidCode string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(v)
	if err == nil && decoder.More() {
		err = errors.New("body must contain one JSON value")
	}
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		return true
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", fmt.Sprintf("request body exceeds %d bytes", maxJSONBodyBytes))
	default:
		writeError(w, http.StatusBadRequest, invalidCode, "body must be one valid JSON object: "+err.Error())
	}
	return false
}

// pathUUID reads the {name} path segment as a UUID, or refuses it with 400 code: an id that is not
// a UUID names nothing, and must never reach a uuid column as a Postgres type error.
func pathUUID(w http.ResponseWriter, r *http.Request, name, code, what string) (string, bool) {
	id := r.PathValue(name)
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, code, what+" ids are UUIDs")
		return "", false
	}
	return id, true
}

// pathRecordID reads the {name} path segment as a credential-request record id, or refuses it
// with 400 code: record ids are lowercase-hex sha256 hashes (record.ValidID), so pathUUID's check
// would wrongly reject every valid one.
func pathRecordID(w http.ResponseWriter, r *http.Request, name, code string) (string, bool) {
	id := r.PathValue(name)
	if !record.ValidID(id) {
		writeError(w, http.StatusBadRequest, code, "record ids are lowercase-hex sha256 hashes")
		return "", false
	}
	return id, true
}

// requireApprover refuses an approver login that canonicalizes to nothing with 400
// APPROVER_REQUIRED: every UI route names the human it acts for, whether a decision's or a
// revoke's approver field or a list's ?approver= query. A login spelled as a group
// (record.GroupOf) is no human either, and is refused 400 APPROVER_IS_GROUP: a group's records and
// grants are listed and revoked by naming a member, whom the group reader admits, never by naming
// the group.
func requireApprover(w http.ResponseWriter, login string) bool {
	canonical := record.CanonicalLogin(login)
	if canonical == "" {
		writeError(w, http.StatusBadRequest, "APPROVER_REQUIRED", "approver is required")
		return false
	}
	if _, group := record.GroupOf(canonical); group {
		writeError(w, http.StatusBadRequest, "APPROVER_IS_GROUP", "approver names a person, not a group")
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"code": code, "error": msg})
}

// pageRefusal is the body of a refusal that names the approval page where a person decides the
// record instead, beside the code and error every refusal carries. On a broker with
// BROKER_SIGNIN=off, which serves no page, it names none and says why.
type pageRefusal struct {
	// The refusal's code.
	Code string `json:"code"`
	// What was refused, naming the page, or, on a broker with BROKER_SIGNIN=off, saying that it
	// serves none.
	Error string `json:"error"`
	// The record's page on the broker's own approval page, <BROKER_PUBLIC_URL>/approve/<record>;
	// absent on a broker with BROKER_SIGNIN=off, where that page is a 404.
	ApproveURL string `json:"approve_url,omitempty"`
}

// writePageRefusal is writeError for a refusal naming the record's approval page, approveURL,
// which its body carries as approve_url (pageRefusal) for a caller to link; an empty approveURL,
// a broker that serves no page, leaves approve_url out.
func writePageRefusal(w http.ResponseWriter, status int, code, msg, approveURL string) {
	writeJSON(w, status, pageRefusal{Code: code, Error: msg, ApproveURL: approveURL})
}

// approveURL is recordID's page on the broker's own approval page (GET /approve/{record}), on the
// public URL the broker resolved at boot: the one place a person signed in to the broker decides
// a secret request, which every answer naming a pending one links. A broker with
// BROKER_SIGNIN=off builds no page (Deps.Approval nil), where that path is a 404, so it answers
// "" and every answer leaves approve_url out.
func (s *server) approveURL(recordID string) string {
	if s.deps.Approval == nil {
		return ""
	}
	return s.deps.PublicURL + "/approve/" + recordID
}

// pendingApproveURL is approveURL for a request's record while the request waits on a person, and
// "" for a request granted or denied at once (no record), once it is no longer pending, and while
// the model judge holds it (requests.Request.JudgeDeciding), since no person decides it then.
func (s *server) pendingApproveURL(req requests.Request) string {
	if req.State != "pending" || req.RecordID == nil || req.JudgeDeciding {
		return ""
	}
	return s.approveURL(*req.RecordID)
}

// writeInternal logs a failed operation with its error and answers 500 INTERNAL naming the
// operation. Errors on these paths carry no secret values; the log line is the only place the
// cause of a 500 is recorded.
func writeInternal(w http.ResponseWriter, op string, err error) {
	slog.Error("broker: "+op+" failed", "error", err)
	writeError(w, http.StatusInternalServerError, "INTERNAL", op+" failed")
}

// writeUnavailable logs a dependency that could not answer (logUnavailable) and answers 503 with
// code naming it (answerUnavailable).
func writeUnavailable(w http.ResponseWriter, code, op string, err error) {
	logUnavailable(op, err)
	answerUnavailable(w, code, op)
}

// logUnavailable writes the ERROR line of a dependency op could not reach: "broker: <op> failed",
// with err and any attrs after it.
func logUnavailable(op string, err error, attrs ...any) {
	slog.Error("broker: "+op+" failed", append([]any{"error", err}, attrs...)...)
}

// answerUnavailable answers 503 with code naming the dependency op could not reach, and writes no
// line.
func answerUnavailable(w http.ResponseWriter, code, op string) {
	writeError(w, http.StatusServiceUnavailable, code, op+" failed: a dependency the broker needs is unavailable")
}

// writeMemberKindUncheckable answers a refusal the broker made because it could not learn whether
// Dispatch calls a person a restricted member (*projectaccess.MemberKindUncheckable), and reports
// whether err was one: 503 MEMBER_KIND_UNCHECKABLE naming the person and why in fixed words, with
// the failure itself, which can name Dispatch's address, in the log alone. what is the thing
// refused, as the log line names it. The line is ERROR when Dispatch could not be asked. A read
// that failed because its caller gave up (MemberKindUncheckable.CallerEnded), which says nothing
// of Dispatch, writes no line and is counted as an answer the caller cancelled
// (answer.Recorder.NoteCallerEnded), as every route counts a caller that hung up. The code is
// projectaccess.UncheckableCode, spelled out here because the broker's generated HTTP reference
// (cmd/cipher-refgen) reads each refusal's code from a literal;
// TestAnUncheckableMemberKindIsAnsweredUnderItsOneCode holds the two to one spelling.
func writeMemberKindUncheckable(w http.ResponseWriter, what string, err error) bool {
	var uncheckable *projectaccess.MemberKindUncheckable
	if !errors.As(err, &uncheckable) {
		return false
	}
	if uncheckable.CallerEnded() {
		answer.FromWriter(w).NoteCallerEnded()
	} else {
		slog.Error("broker: whether Dispatch calls a person a restricted member cannot be read, so "+what+" is refused",
			"person", uncheckable.Person, "error", uncheckable.Err)
	}
	writeError(w, http.StatusServiceUnavailable, "MEMBER_KIND_UNCHECKABLE", uncheckable.Error())
	return true
}

// writeJSON answers status with v, which is one of this package's named response types: the
// broker's generated HTTP reference (cmd/cipher-refgen) documents each route's answer
// from that type's fields and refuses a value it cannot name.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// stateResponse is what a denial, a cancel or a revoke answers: wire.StateResponse itself, named
// here because the broker's generated HTTP reference documents a route's answer from a type this
// package declares, as introspectionResponse names oauthwire.Introspection
// (handlers_introspect.go).
type stateResponse = wire.StateResponse

// healthResponse is GET /healthz's answer while the broker is not draining, its proxy holds an
// intermediate and it can reach Postgres. No refusal has a type of its own: each is the ordinary
// broker error object writeError writes, so this stays the route's one named response type.
type healthResponse struct {
	// "ok".
	Status string `json:"status"`
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	// A draining pod fails its health check before anything else, the Postgres ping included: the
	// answer has to flip the moment the process begins stopping, whatever the database is doing,
	// because this is what takes the pod out of its Service's endpoints while it goes on serving
	// the requests already routed to it. The refusal is an ordinary broker error object rather
	// than a healthResponse with some other status, so a caller reads the same {code, error}
	// shape every other refusal has.
	if s.deps.Draining != nil && s.deps.Draining() {
		writeError(w, http.StatusServiceUnavailable, "SHUTTING_DOWN", "the broker is shutting down and is no longer taking new traffic")
		return
	}
	// Then a proxy with nothing to sign a bound host's leaf with (Deps.ProxyReady), before the
	// ping: such a pod would fail every bound handshake whatever its database says.
	if s.deps.ProxyReady != nil {
		if err := s.deps.ProxyReady(); err != nil {
			writeError(w, http.StatusServiceUnavailable, "NO_INTERMEDIATE", err.Error())
			return
		}
	}
	if err := s.deps.pingDatabase(r.Context()); err != nil {
		if s.pingLogsItself {
			answerUnavailable(w, "DATABASE_UNAVAILABLE", "ping Postgres")
			return
		}
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "ping Postgres", err)
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// healthCacheFor is how long the edge's GET /healthz answers from one Postgres ping: the load
// balancer's health check asks every few seconds, so a second is fresh enough for it, and a caller
// on the internet who asks at any rate costs the database at most one ping a second per pod.
const healthCacheFor = time.Second

// healthPingTimeout bounds one ping of the edge's health cache (cachedPing): a database that
// answers no ping in two seconds is one the load balancer should take the pod out for, and the
// caller that runs the ping waits no longer than that after its own request ends.
const healthPingTimeout = 2 * time.Second

// cachedPing is a Postgres ping whose result is kept for healthCacheFor: the edge's
// Deps.DatabasePing (RegisterEdge). At most one ping runs at a time, on a context of its own
// (context.WithoutCancel of the caller's, with timeout), so a caller that leaves mid-ping neither
// cancels it, which in pgx closes the pool's connection, nor keeps its result from the next
// caller. Its result is kept and, when it failed, logged with one ERROR line naming port
// (answer.PortEdge), whoever waited for it: a failing or hung database writes one line a ping, at
// most one per healthCacheFor plus timeout per pod, whatever the number of callers. A caller that
// asks while a ping runs waits for it, and leaves as soon as its own request ends.
type cachedPing struct {
	ping func(context.Context) error
	now  func() time.Time
	// port is the value of the port field the line of a failed ping names (answer.PortEdge).
	port string
	// timeout bounds one ping; zero means healthPingTimeout.
	timeout time.Duration

	mu  sync.Mutex
	at  time.Time
	err error
	// running is closed when the ping that runs now ends; nil while none runs.
	running chan struct{}
}

// result is the last ping's result while it is under healthCacheFor old, and otherwise the result
// of a ping it runs (run) or waits for. A waiter whose request ends first answers its own
// context's error; the ping goes on, and its result is kept for the next caller.
func (c *cachedPing) result(ctx context.Context) error {
	for {
		c.mu.Lock()
		if now := c.now(); !c.at.IsZero() && now.Sub(c.at) < healthCacheFor {
			err := c.err
			c.mu.Unlock()
			return err
		}
		if wait := c.running; wait != nil {
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		done := make(chan struct{})
		c.running = done
		c.mu.Unlock()
		return c.run(ctx, done)
	}
}

// run is the one ping running now, which result started: on context.WithoutCancel(ctx) with the
// ping's own timeout, its result kept and, when it failed, logged. It clears running and closes
// done in a defer, so a ping that panics leaves the next caller to ping again rather than wait on
// a ping that will never end.
func (c *cachedPing) run(ctx context.Context, done chan struct{}) error {
	defer func() {
		c.mu.Lock()
		c.running = nil
		c.mu.Unlock()
		close(done)
	}()
	timeout := c.timeout
	if timeout == 0 {
		timeout = healthPingTimeout
	}
	pingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	err := c.ping(pingCtx)
	c.mu.Lock()
	c.at, c.err = c.now(), err
	c.mu.Unlock()
	if err != nil {
		logUnavailable("ping Postgres", err, "port", c.port)
	}
	return err
}

// signInStatusResponse is GET /signin/status's answer, the broker's one signin_status row as it
// stands. Every field is null for a fact that has never happened, so a reader never has to tell
// a zero time from a real one, and no field holds free text: the two words that are not times
// come from closed sets Postgres itself holds the broker to.
type signInStatusResponse struct {
	// What the last check of the broker's client at its sign-in provider found: "ready" (the
	// provider accepted the client), "client_rejected" (it refused it, so nobody can sign in)
	// or "issuer_unreachable" (it could not be asked or gave no verdict, which says nothing about
	// the client).
	ClientCheck *string `json:"client_check"`
	// When that check was taken (RFC 3339).
	ClientCheckedAt *time.Time `json:"client_checked_at"`
	// When a sign-in last completed.
	LastSignInAt *time.Time `json:"last_sign_in_at"`
	// When a sign-in last failed at the token exchange or at the ID-token check.
	LastFailedSignInAt *time.Time `json:"last_failed_sign_in_at"`
	// Why that sign-in failed, one of invalid_client, unauthorized_client,
	// redirect_uri_mismatch, invalid_grant, issuer_unreachable, bad_issuer, bad_audience,
	// bad_signature, bad_nonce, not_in_domain and email_not_verified.
	LastFailedSignInReason *string `json:"last_failed_sign_in_reason"`
	// When a decision was last made on the approval page.
	LastDecisionAt *time.Time `json:"last_decision_at"`
}

// signInStatus answers the broker's sign-in status to any caller. It is read from Postgres on
// every call rather than from this process's memory, so every replica and every restart answers
// the same thing. It spends its source address's bucket first, the one the approval page's
// routes and the routes a ServiceAccount token authenticates share (accountAddressAllows,
// DefaultAccountLimit), so no caller drives this read unmetered; the route takes no credential,
// so nothing else bounds it. Being JSON and public, it refuses in JSON, but names the address
// rather than the ServiceAccount token refuseAccountAddress names: this caller sent none.
func (s *server) signInStatus(w http.ResponseWriter, r *http.Request) {
	if !s.accountAddressAllows(r) {
		refuseWithRetryAfter(w, s.accountLimiter.Every(), "too many requests from this address; try again later")
		return
	}
	status, err := s.deps.Enroll.Store.SignInStatus(r.Context())
	if err != nil {
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "read the sign-in status", err)
		return
	}
	writeJSON(w, http.StatusOK, signInStatusResponse{
		ClientCheck:            status.ClientCheckState,
		ClientCheckedAt:        status.ClientCheckedAt,
		LastSignInAt:           status.LastSignInAt,
		LastFailedSignInAt:     status.LastFailedSignInAt,
		LastFailedSignInReason: status.LastFailedSignInReason,
		LastDecisionAt:         status.LastDecisionAt,
	})
}

// approvalStylesheet serves the approval page's stylesheet (approval.ServeStylesheet) with no
// credential, since every page it dresses is read before anyone has signed in.
func (s *server) approvalStylesheet(w http.ResponseWriter, r *http.Request) {
	approval.ServeStylesheet(w, r)
}

// strPtr is nil for "" and &s otherwise, for an optional wire field that is null rather than "".
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
