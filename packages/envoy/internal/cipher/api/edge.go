package api

import (
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"authelia.com/provider/oauth2"
	"github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// Edge is the broker's edge as RegisterEdge mounts it: the listener BROKER_EDGE_LISTEN_ADDR binds,
// which a caller outside the deployment's private network reaches through a public load balancer,
// and which serves the device login and nothing else of the broker.
type Edge struct {
	// URL is BROKER_EDGE_URL, the broker's address as a caller of the edge reaches it: the
	// audience a login's request object made there names, and the URL every DPoP proof sent
	// there names with its path, in place of BROKER_PUBLIC_URL.
	URL string
	// VerificationURL is BROKER_EDGE_MACHINE_LOGIN_URL, the page a device authorization made at
	// the edge sends its person to.
	VerificationURL string
	// WorkspaceDomains are the domains the Google Workspace owns: BROKER_SIGNIN_DOMAIN and each
	// domain BROKER_SIGNIN_WORKSPACE_DOMAINS lists, each a lowercase domain name as its A-labels.
	// A device authorization naming an address on one of them, or on a subdomain of one, is
	// refused before it spends anything (refuseWorkspaceLogin).
	WorkspaceDomains []string
}

// edgeLoginKeyPrefix opens the key of every per-login bucket the edge spends, so a key the edge
// writes never names a bucket the broker's own address spends, as the separate buckets the edge's
// server holds already ensure; the prefix makes that true of the keys themselves.
const edgeLoginKeyPrefix = "edge:"

// edgeRoutes are the rows of routes() the edge mounts, by method and pattern, each with the gate
// it runs first: the device authorization and the token endpoint, gated on the caller's address,
// on a form body and on what each refuses at the edge, and the health check, which the load
// balancer's own probe calls with no address to read and spends nothing.
var edgeRoutes = map[string]func(Edge, http.ResponseWriter, *http.Request) bool{
	http.MethodPost + " " + deviceAuthorizationPath: edgeDeviceGate,
	http.MethodPost + " " + tokenPath:               edgeTokenGate,
	http.MethodGet + " /healthz":                    nil,
}

// RegisterEdge mounts the broker's edge on mux: of the broker's route table, POST
// /v1/oauth/device_authorization, POST /v1/oauth/token and GET /healthz, and no other, each
// served by the handler the broker's own address serves it with, on a server of the edge's own.
// That server's buckets are its own, so a flood at the edge spends nothing a caller of the
// broker's own address is held to; every bucket keys on the address edgeClientAddress reads, and
// every per-login key opens with edgeLoginKeyPrefix. deps is the broker's own (Register's), and
// under it the edge holds a login's request object and every DPoP proof to edge.URL, and answers
// edge.VerificationURL as a device authorization's verification_uri. Before either login route's
// handler runs, the edge refuses a request whose address it cannot read, a body that is not a
// form, a device authorization naming a person of the Workspace and a grant the outside agent
// does not trade (edgeDeviceGate, edgeTokenGate).
//
// It panics on a deps the edge cannot serve, as Register does: no OAuth provider or one serving no
// device grant, either of whose routes the edge would then answer with nothing, and a
// machine-login service, request machine or launcher credentials' chain that does not take
// edge.URL as the address of a record it reads back, so a login the edge opened could never be
// listed, looked up, decided or used (record.VerifyStoredRequestObject).
func RegisterEdge(mux *http.ServeMux, deps Deps, edge Edge) {
	switch {
	case deps.OAuth == nil || !deps.OAuth.DeviceGrant():
		panic("api: the edge serves the device grant, so it needs an OAuth provider with a verification URL")
	case deps.MachineLogin == nil || deps.DeviceLogin == nil || deps.TokenHooks.Before == nil || deps.TokenHooks.After == nil:
		panic("api: Deps.OAuth needs MachineLogin, DeviceLogin and both TokenHooks")
	case deps.MachineLogin.EdgeAudience != edge.URL || deps.Machine == nil || deps.Machine.EdgeAudience != edge.URL ||
		deps.Enroll == nil || deps.Enroll.Chain == nil || deps.Enroll.Chain.EdgeAudience != edge.URL:
		panic("api: the machine-login service, the request machine and the launcher credentials' chain must each take the edge's URL as their EdgeAudience")
	}
	login := deps.MachineLogin.AtEdge()
	edgeDeps := deps
	edgeDeps.PublicURL = edge.URL
	edgeDeps.MachineLogin = login
	edgeDeps.DeviceLogin = login.LoginDevice
	limits := DefaultLauncherLimits
	if deps.LauncherLimits != nil {
		limits = *deps.LauncherLimits
	}
	token := DefaultTokenLimit
	if deps.TokenLimit != nil {
		token = *deps.TokenLimit
	}
	// The gates have refused every request edgeClientAddress reads no address from before a
	// bucket is reached, so the answer dropped here is never false.
	address := func(r *http.Request) string { address, _ := edgeClientAddress(r); return address }
	s := &server{
		deps:            edgeDeps,
		launcherLimiter: newLauncherLimiter(limits, address),
		tokenLimiter:    ratelimit.NewKeyed(token),
		address:         address,
		loginKeyPrefix:  edgeLoginKeyPrefix,
		verificationURI: edge.VerificationURL,
	}
	for _, route := range routes() {
		pattern := route.Method + " " + route.Pattern
		gate, mounted := edgeRoutes[pattern]
		if !mounted {
			continue
		}
		next := s.handle(route)
		if gate == nil {
			mux.HandleFunc(pattern, next)
			continue
		}
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if gate(edge, w, r) {
				next(w, r)
			}
		})
	}
}

// edgeDeviceGate is what the edge checks of a device authorization before its handler runs, and
// reports whether the handler may: the caller's address and the body's form (edgeFormGate), and
// then that the person the request object's login_hint names is no person of the Workspace
// (refuseWorkspaceLogin). Each refusal comes before any bucket is spent or any row is written.
func edgeDeviceGate(edge Edge, w http.ResponseWriter, r *http.Request) bool {
	return edgeFormGate(w, r) && !refuseWorkspaceLogin(w, r.PostForm.Get("request"), edge.WorkspaceDomains)
}

// edgeTokenGate is what the edge checks of a token request before its handler runs, and reports
// whether the handler may: the caller's address and the body's form (edgeFormGate), and then
// that the request is one of the grants an agent outside the deployment trades (edgeGrant). A
// grant it does not take is 400 unsupported_grant_type, naming EDGE_GRANT_REFUSED, before any
// bucket is spent: the pod's, the workflow run's and the Google trades are the deployment's
// own, and their callers reach the broker on its own address.
func edgeTokenGate(_ Edge, w http.ResponseWriter, r *http.Request) bool {
	if !edgeFormGate(w, r) {
		return false
	}
	if !edgeGrant(r.PostForm) {
		oauth.WriteRefusal(w, &oauth.Refusal{Status: http.StatusBadRequest, OAuthError: "unsupported_grant_type", Code: "EDGE_GRANT_REFUSED",
			Message: "the broker's edge takes the device code, a refresh token and the trade of one of its own access tokens with no actor token, and no other grant"})
		return false
	}
	return true
}

// edgeFormGate refuses, 400 invalid_request REQUEST_INVALID, a login request the edge cannot
// judge: one whose X-Forwarded-For names no client address (edgeClientAddress), one whose body is
// not application/x-www-form-urlencoded, and one whose form does not parse within
// maxOAuthFormBytes (readOAuthForm). It reports whether the request passed, its form parsed.
//
// The body's type is held because the provider's library reads a multipart body as a form too,
// after the broker's own parse has read nothing of it, so a gate that judged the form the broker
// parsed would judge an empty one while the provider served what the multipart body named.
func edgeFormGate(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := edgeClientAddress(r); !ok {
		oauth.WriteRefusal(w, &oauth.Refusal{Status: http.StatusBadRequest, Code: "REQUEST_INVALID",
			Message: "the broker's edge reads its caller's address from the second-to-last X-Forwarded-For entry, the one its load balancer appends, and this request carries no such entry"})
		return false
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/x-www-form-urlencoded" {
		oauth.WriteRefusal(w, &oauth.Refusal{Status: http.StatusBadRequest, Code: "REQUEST_INVALID",
			Message: "the broker's edge takes a body of type application/x-www-form-urlencoded and no other"})
		return false
	}
	return readOAuthForm(w, r)
}

// edgeGrant reports whether form, a token request's, is one of the grants an agent outside the
// deployment trades at the edge: one grant_type, which is the device code, a refresh token, or
// the RFC 8693 trade of one of the broker's own access tokens (subject_token_type
// urn:ietf:params:oauth:token-type:access_token, given once) with no actor token or actor token
// type at all. Every other grant, every other subject token type, an actor of any kind and a
// parameter given twice are refused.
func edgeGrant(form url.Values) bool {
	grants := form["grant_type"]
	if len(grants) != 1 {
		return false
	}
	switch oauth2.GrantType(grants[0]) {
	case oauth2.GrantTypeDeviceCode, oauth2.GrantTypeRefreshToken:
		return true
	case oauth2.GrantTypeTokenExchange:
		types := form[oauth.FormSubjectTokenType]
		_, actor := form["actor_token"]
		_, actorType := form["actor_token_type"]
		return len(types) == 1 && types[0] == oauthwire.SubjectTokenTypeAccessToken && !actor && !actorType
	}
	return false
}

// refuseWorkspaceLogin writes 400 invalid_request EDGE_WORKSPACE_LOGIN and reports true when the
// request object compact claims a login_hint that is a person of the Workspace: an address on one
// of domains or a subdomain of one, as Dispatch's sign-in reads an address by the same domains
// (oidc.OnWorkspaceDomain), or a login that is no address at all, which names no person outside
// it. It reads the claim without verifying the object's signature, which costs no signature check
// and no bucket: a refusal only ever refuses the caller who sent it, and the object the gate passes
// is verified as on the broker's own address. An object it cannot read, or one claiming no
// login_hint, is the handler's to refuse, as it is there.
func refuseWorkspaceLogin(w http.ResponseWriter, compact string, domains []string) bool {
	login := record.CanonicalLogin(claimedLoginHint(compact))
	if login == "" {
		return false
	}
	at := strings.LastIndexByte(login, '@')
	if at >= 0 && !oidc.OnWorkspaceDomain(login[at+1:], domains) {
		return false
	}
	oauth.WriteRefusal(w, &oauth.Refusal{Status: http.StatusBadRequest, Code: "EDGE_WORKSPACE_LOGIN",
		Message: "the broker's edge opens a machine login only for a person outside the Workspace; a person of the Workspace logs a machine in on the broker's own address"})
	return true
}

// claimedLoginHint is the login_hint compact, a request object, claims, read with no check of its
// signature, and "" for an object that is no ES256 JWS or whose payload names none.
func claimedLoginHint(compact string) string {
	signed, err := jose.ParseSigned(compact, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return ""
	}
	var claims struct {
		LoginHint string `json:"login_hint"`
	}
	if json.Unmarshal(signed.UnsafePayloadWithoutVerification(), &claims) != nil {
		return ""
	}
	return claims.LoginHint
}
