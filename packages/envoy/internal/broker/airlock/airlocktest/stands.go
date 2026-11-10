// packages/envoy/internal/broker/airlock/airlocktest/stands.go
// The eight stand-ins a Service answers as: GitHub's App endpoints, Google's token endpoint,
// Microsoft's authority, an AWS service that checks the signature it was sent, the WebSocket
// upgrade a bound host answers, and the three the broker's model judge calls — a model
// endpoint, Dispatch's two issue reads, and the Compute Engine metadata server a machine reads
// its own identity token from. Each is a value holding what it answers and what it was asked,
// and each installs its Answer on a Service (Service.AnswerWith), so one of them may play a
// host of its own in the command and the same one may play the bound host of a row that
// already has a service.
package airlocktest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/broker/requests"
)

// GitHubOptions is what a GitHubStand answers with.
type GitHubOptions struct {
	// Installations is the installation id the App has on each owner, by owner. A mint for an
	// owner no entry names finds no installation, which is what the minter's own refusal is
	// about.
	Installations map[string]int64
	// Token is the installation token every answer carries, and "" is a token of each owner's
	// own, ghs_standin_<owner>, so a row that mints for two owners can tell the two apart.
	Token string
	// Lifetime is how long from now each token's expires_at is, and zero is an hour, the life
	// GitHub gives one.
	Lifetime time.Duration
}

// GitHubStand is the two routes secrets.GitHubApp calls: GET /app/installations, which answers
// the installations it was given, and POST /app/installations/{id}/access_tokens, which answers
// that installation's token and its expiry. Pointing BROKER_AIRLOCK_TEST_GITHUB_API_BASE at a
// Service answering as one is what makes a github-app secret mint against this process.
type GitHubStand struct {
	options GitHubOptions
}

// NewGitHubStand builds one. Install it with Service.AnswerWith(stand.Answer).
func NewGitHubStand(o GitHubOptions) *GitHubStand { return &GitHubStand{options: o} }

// owners names every owner the App is installed on, in order, so two runs answer one list.
func (g *GitHubStand) owners() []string {
	owners := make([]string, 0, len(g.options.Installations))
	for owner := range g.options.Installations {
		owners = append(owners, owner)
	}
	slices.Sort(owners)
	return owners
}

// token is the installation token this stand-in answers for owner.
func (g *GitHubStand) token(owner string) string {
	if g.options.Token != "" {
		return g.options.Token
	}
	return "ghs_standin_" + owner
}

// Answer is GitHub's answer to each of the two routes, and a 404 naming anything else: a minter
// that called a route this stand-in does not serve reads which one rather than an empty answer.
func (g *GitHubStand) Answer(w http.ResponseWriter, _ *http.Request, seen Request) bool {
	lifetime := g.options.Lifetime
	if lifetime <= 0 {
		lifetime = time.Hour
	}
	w.Header().Set("Content-Type", "application/json")
	switch owner, route := g.route(seen); route {
	case "installations":
		installations := []map[string]any{}
		for _, owner := range g.owners() {
			installations = append(installations, map[string]any{
				"id":      g.options.Installations[owner],
				"account": map[string]string{"login": owner},
			})
		}
		_ = json.NewEncoder(w).Encode(installations)
	case "token":
		// 201 and an expires_at: the minter refuses any other status and refuses a token with
		// no expiry, so an answer short of both is no answer at all.
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      g.token(owner),
			"expires_at": time.Now().Add(lifetime).UTC().Format(time.RFC3339),
		})
	default:
		http.Error(w, "the GitHub stand-in serves GET /app/installations and POST /app/installations/{id}/access_tokens, not "+seen.Method+" "+seen.Path, http.StatusNotFound)
	}
	return true
}

// route reads which of the two routes a request is, and for a token request the owner whose
// installation id it names. An id no owner holds is no route of this stand-in's: a minter that
// asked for a token of an installation nothing was configured for reads the 404 above rather
// than a token of nobody's.
func (g *GitHubStand) route(seen Request) (owner, route string) {
	if seen.Method == http.MethodGet && seen.Path == "/app/installations" {
		return "", "installations"
	}
	rest, isToken := strings.CutSuffix(strings.TrimPrefix(seen.Path, "/app/installations/"), "/access_tokens")
	if seen.Method != http.MethodPost || !isToken {
		return "", ""
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return "", ""
	}
	for _, owner := range g.owners() {
		if g.options.Installations[owner] == id {
			return owner, "token"
		}
	}
	return "", ""
}

// GoogleOptions is what a GoogleStand answers with.
type GoogleOptions struct {
	// AccessToken is the token each exchange answers, and "" is a stand-in token of its own.
	AccessToken string
	// Lifetime is the answer's expires_in, and zero is an hour. A check that drives a minted
	// value to within the proxy's five-minute margin names a lifetime of minutes here.
	Lifetime time.Duration
}

// GoogleStand is the token endpoint secrets.GoogleLogin exchanges a refresh token at: it answers
// {"access_token", "expires_in"}, the two fields the minter reads, and records every form it was
// sent (the Service's own record). BROKER_AIRLOCK_TEST_GOOGLE_TOKEN_URL points a google-login
// secret's minter at it.
type GoogleStand struct {
	options GoogleOptions
	// refreshes numbers the tokens this stand has answered, so two mints are told apart.
	refreshes atomic.Int64
}

// NewGoogleStand builds one. Install it with Service.AnswerWith(stand.Answer).
func NewGoogleStand(o GoogleOptions) *GoogleStand { return &GoogleStand{options: o} }

// Answer is Google's answer to one refresh.
func (g *GoogleStand) Answer(w http.ResponseWriter, _ *http.Request, seen Request) bool {
	if seen.Method != http.MethodPost {
		http.Error(w, "the Google stand-in token endpoint takes POST, not "+seen.Method, http.StatusMethodNotAllowed)
		return true
	}
	form, err := formOf(seen)
	if err != nil || form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_request"})
		return true
	}
	// A fresh token for each refresh, numbered, so a caller can tell one mint from the next:
	// Google answers a new access token every time, and a check that a value was minted again
	// cannot read a stand-in that answers one fixed string. AccessToken fixes it where a row
	// wants the same answer twice.
	token := g.options.AccessToken
	if token == "" {
		token = fmt.Sprintf("the-stand-in-google-access-token-%d", g.refreshes.Add(1))
	}
	lifetime := g.options.Lifetime
	if lifetime <= 0 {
		lifetime = time.Hour
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": token,
		"expires_in":   int64(lifetime / time.Second),
		"token_type":   "Bearer",
	})
	return true
}

// MicrosoftOptions is what a MicrosoftStand answers with.
type MicrosoftOptions struct {
	// Tokens is the access token each exchange answers in turn; the last one answers every
	// exchange after it. None is one stand-in token for every exchange.
	Tokens []string
	// IDToken is the id_token the answer carries beside the access token, so a row can pin
	// that the proxy hands the command no token but the access one. None leaves the field out
	// altogether, as Microsoft's own answer to a client-credentials exchange leaves it out: a
	// client's library reads an id_token that is there and an empty one makes it fail.
	IDToken string
	// Assertion is the client_assertion the endpoint accepts, and "" accepts any: an exchange
	// carrying another assertion is answered 401, as Microsoft answers an assertion that is not
	// the application's.
	Assertion string
	// Lifetime is the answer's expires_in, and zero is 3599 seconds, the life Microsoft gives
	// an access token.
	Lifetime time.Duration
	// Refuse is the status every exchange is refused with, and zero answers them.
	Refuse int
}

// MicrosoftStand is the authority the azure kind exchanges a federated token at: it answers
// the tenant's OpenID configuration document, which is the first thing a client reads of an
// authority, and the token endpoint itself, recording the client_assertion each exchange
// carried - readable back through Exchanges and in the Service's own record - and answering an
// access token. It answers those two alone and leaves every other request to the service's own
// body, because the host it stands in for serves an API beside its token endpoint and a row
// drives both.
type MicrosoftStand struct {
	options MicrosoftOptions

	mu     sync.Mutex
	forms  []url.Values
	tokens []string
	refuse int
}

// NewMicrosoftStand builds one. Install it with Service.AnswerWith(stand.Answer).
func NewMicrosoftStand(o MicrosoftOptions) *MicrosoftStand {
	return &MicrosoftStand{options: o, tokens: o.Tokens, refuse: o.Refuse}
}

// Answers replaces the access tokens the endpoint answers in turn from here on, which is how a
// row says that the next exchange is answered a second token.
func (m *MicrosoftStand) Answers(tokens ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens = tokens
}

// Refuse makes every exchange from here on answer status, and zero answers them again.
func (m *MicrosoftStand) Refuse(status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refuse = status
}

// isTokenRequest reports whether a request is one to a token endpoint: Microsoft's are
// /{tenant}/oauth2/v2.0/token and /{tenant}/oauth2/token. It is the shape airlockproxy's own
// isTokenRequest reads, written again here because that package's tests import this one and the
// two cannot both import the other.
func isTokenRequest(seen Request) bool {
	return seen.Method == http.MethodPost && strings.Contains(seen.Path, "/oauth2/") && strings.HasSuffix(seen.Path, "/token")
}

// openIDConfigurationSuffix is what a tenant's OpenID configuration document is served under,
// after the tenant itself: GET /{tenant}/v2.0/.well-known/openid-configuration. It is the
// first call `az login` makes against an authority, before any token request, and a client
// that cannot read it never gets as far as the exchange.
const openIDConfigurationSuffix = "/v2.0/.well-known/openid-configuration"

// configurationTenant is the tenant whose OpenID configuration a request asks for, and false
// for every other request. The tenant is one path segment, as Microsoft's authorities are.
func configurationTenant(seen Request) (string, bool) {
	if seen.Method != http.MethodGet {
		return "", false
	}
	tenant, ok := strings.CutSuffix(seen.Path, openIDConfigurationSuffix)
	if !ok {
		return "", false
	}
	tenant = strings.TrimPrefix(tenant, "/")
	if tenant == "" || strings.Contains(tenant, "/") {
		return "", false
	}
	return tenant, true
}

// configuration writes the tenant's OpenID configuration document, every endpoint of it on
// this stand's own base URL - the scheme and authority the request arrived on - so a client
// that reads the document makes every call after it against this process and none against
// Microsoft. The fields are the ones the Azure CLI's own library reads of it; a field it does
// not read is here because the document it reads from Microsoft carries it and a client that
// compares what it got with what it expected should find no difference.
func configuration(w http.ResponseWriter, seen Request, tenant string) {
	base := "https://" + seen.Host + "/" + tenant
	doc := map[string]any{
		"issuer":                                base + "/v2.0",
		"authorization_endpoint":                base + "/oauth2/v2.0/authorize",
		"token_endpoint":                        base + "/oauth2/v2.0/token",
		"device_authorization_endpoint":         base + "/oauth2/v2.0/devicecode",
		"end_session_endpoint":                  base + "/oauth2/v2.0/logout",
		"jwks_uri":                              base + "/discovery/v2.0/keys",
		"userinfo_endpoint":                     "https://" + seen.Host + "/oidc/userinfo",
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "private_key_jwt", "client_secret_basic"},
		"response_modes_supported":              []string{"query", "fragment", "form_post"},
		"response_types_supported":              []string{"code", "id_token", "code id_token", "id_token token"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token", "client_credentials", "implicit", "urn:ietf:params:oauth:grant-type:device_code"},
		"scopes_supported":                      []string{"openid", "profile", "email", "offline_access"},
		"subject_types_supported":               []string{"pairwise"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"claims_supported": []string{
			"sub", "iss", "aud", "exp", "iat", "auth_time", "acr", "nonce", "preferred_username",
			"name", "tid", "ver", "at_hash", "c_hash", "email",
		},
		"request_uri_parameter_supported": false,
		"http_logout_supported":           true,
		"frontchannel_logout_supported":   true,
		// The three a cloud's own metadata is read from, each this stand's own: the CLI reads
		// them to tell which cloud it is speaking to.
		"tenant_region_scope":   "NA",
		"cloud_instance_name":   seen.Host,
		"cloud_graph_host_name": seen.Host,
		"msgraph_host":          seen.Host,
		"rbac_url":              "https://" + seen.Host,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(doc)
}

// Answer is Microsoft's answer to one exchange and to a tenant's OpenID configuration, and
// false for any other request.
func (m *MicrosoftStand) Answer(w http.ResponseWriter, _ *http.Request, seen Request) bool {
	if tenant, ok := configurationTenant(seen); ok {
		configuration(w, seen, tenant)
		return true
	}
	if !isTokenRequest(seen) {
		return false
	}
	form, err := formOf(seen)
	if err != nil {
		http.Error(w, "the token endpoint read no form", http.StatusBadRequest)
		return true
	}
	m.mu.Lock()
	m.forms = append(m.forms, form)
	exchanges, refuse := len(m.forms), m.refuse
	token := "the-stand-in-microsoft-access-token"
	if len(m.tokens) > 0 {
		token = m.tokens[min(exchanges, len(m.tokens))-1]
	}
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if refuse != 0 {
		w.WriteHeader(refuse)
		fmt.Fprint(w, `{"error":"invalid_client","error_codes":[5002723]}`)
		return true
	}
	if m.options.Assertion != "" && form.Get("client_assertion") != m.options.Assertion {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid_client","error_description":"the assertion is not this application's"}`)
		return true
	}
	lifetime := m.options.Lifetime
	if lifetime <= 0 {
		lifetime = 3599 * time.Second
	}
	seconds := int64(lifetime / time.Second)
	idToken := ""
	if m.options.IDToken != "" {
		idToken = fmt.Sprintf(`,"id_token":%q`, m.options.IDToken)
	}
	fmt.Fprintf(w, `{"token_type":"Bearer","expires_in":%d,"ext_expires_in":%d,"access_token":%q%s}`,
		seconds, seconds, token, idToken)
	return true
}

// Exchanges is every form the token endpoint was sent, in order.
func (m *MicrosoftStand) Exchanges() []url.Values {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]url.Values(nil), m.forms...)
}

// AzureResourceOptions is what an AzureResourceStand answers with.
type AzureResourceOptions struct {
	// Subscription is the subscription id the stand holds, and "" is one of its own. A client
	// that signs in to a cloud reads the subscriptions of the tenant it signed in to before it
	// will run anything, so the stand holds exactly one.
	Subscription string
	// Tenant is the tenant that subscription is in, and "" is the stand-in tenant.
	Tenant string
}

// AzureResourceStand is the Azure Resource Manager an access token is spent at: the two routes
// a signed-in client reads before it does anything - the tenant's subscriptions, and one
// subscription's resource groups - each answered as the Resource Manager answers them, so a
// client that reached this stand with a token is a client that signed in and ran a command
// rather than one that stopped at a body it could not read.
//
// It answers those routes alone and leaves every other request to the service's own body,
// which is what lets one Service play the authority and the Resource Manager at once
// (Answers).
type AzureResourceStand struct {
	options AzureResourceOptions
}

// NewAzureResourceStand builds one. Install it with Service.AnswerWith(stand.Answer).
func NewAzureResourceStand(o AzureResourceOptions) *AzureResourceStand {
	return &AzureResourceStand{options: o}
}

// Subscription is the subscription id this stand holds.
func (a *AzureResourceStand) Subscription() string {
	if a.options.Subscription == "" {
		return "4b6e1dcd-6f2a-4d0e-9a2c-7f9b0a1c2d3e"
	}
	return a.options.Subscription
}

// Tenant is the tenant that subscription is in.
func (a *AzureResourceStand) Tenant() string {
	if a.options.Tenant == "" {
		return "the-stand-in-tenant"
	}
	return a.options.Tenant
}

// Answer is the Resource Manager's answer to those two routes, and false for every other
// request.
func (a *AzureResourceStand) Answer(w http.ResponseWriter, _ *http.Request, seen Request) bool {
	if seen.Method != http.MethodGet {
		return false
	}
	var doc map[string]any
	switch {
	case seen.Path == "/subscriptions":
		doc = map[string]any{"value": []any{map[string]any{
			"id":             "/subscriptions/" + a.Subscription(),
			"subscriptionId": a.Subscription(),
			"tenantId":       a.Tenant(),
			"displayName":    "the stand-in subscription",
			"state":          "Enabled",
		}}}
	case strings.HasPrefix(seen.Path, "/subscriptions/"):
		// Everything under one subscription - its resource groups, its providers - is empty:
		// what a check drives here is the call and the token it carried, never a resource.
		doc = map[string]any{"value": []any{}}
	default:
		return false
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(doc)
	return true
}

// Answers is the one Answer a Service is given when several stand-ins share it: each in turn
// until one answers, and false when none did, which leaves the service its own body. It is how
// one process plays two hosts of one certificate - the Microsoft authority and the Resource
// Manager an access token is spent at - since a Service holds one Answer.
func Answers(answers ...Answer) Answer {
	return func(w http.ResponseWriter, r *http.Request, seen Request) bool {
		for _, answer := range answers {
			if answer(w, r, seen) {
				return true
			}
		}
		return false
	}
}

// AWSOptions is the key an AWSStand verifies a call with.
type AWSOptions struct {
	// KeyID and SecretKey are the real key pair behind the airlock: the one the broker signs a
	// call again with, and the only one a call this stand-in accepts can have been signed with.
	KeyID, SecretKey string
	// SessionToken is the real session token of a pair that has one, and "" is a pair that has
	// none, where a call carrying any X-Amz-Security-Token is refused.
	SessionToken string
	// Body is what it answers a call that verifies, and "" is a line of its own.
	Body string
}

// AWSStand is a stand-in AWS service: it verifies the Signature Version 4 of every call it is
// sent against the real key pair, answers 403 naming what did not verify when it does not, and
// records what it got (the Service's own record). Nothing of the signer the proxy signs with is
// used here, so a signature this stand-in accepts is one AWS would accept and not one the same
// code agreed with itself about.
type AWSStand struct {
	options AWSOptions

	mu       sync.Mutex
	refusals []string
}

// NewAWSStand builds one. Install it with Service.AnswerWith(stand.Answer).
func NewAWSStand(o AWSOptions) *AWSStand { return &AWSStand{options: o} }

// Answer verifies one call and answers it.
func (a *AWSStand) Answer(w http.ResponseWriter, _ *http.Request, seen Request) bool {
	if err := VerifySigV4(seen, a.options.KeyID, a.options.SecretKey, a.options.SessionToken); err != nil {
		a.mu.Lock()
		a.refusals = append(a.refusals, err.Error())
		a.mu.Unlock()
		http.Error(w, "the stand-in service refused this call: "+err.Error(), http.StatusForbidden)
		return true
	}
	if body := a.options.Body; body != "" {
		fmt.Fprint(w, body)
		return true
	}
	if body, parsed := awsAnswerFor(seen); parsed {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, body)
		return true
	}
	fmt.Fprint(w, "the stand-in service verified the signature")
	return true
}

// awsAnswerFor is the answer its client parses for the calls a check drives through this
// stand-in, and whether there is one. A correctly signed call answered with a line of prose is
// a call the AWS CLI reports as a 500, which hides whether it worked: the signature verified
// and the call still reads as broken. So a list and an identity answer the service's own XML,
// empty, and every other call keeps the line. AWSOptions.Body, where a row sets one, wins over
// both.
func awsAnswerFor(seen Request) (string, bool) {
	const header = `<?xml version="1.0" encoding="UTF-8"?>`
	switch {
	case seen.Method == http.MethodPost && strings.Contains(seen.Body, "Action=GetCallerIdentity"):
		return header + `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">` +
			`<GetCallerIdentityResult><Arn>arn:aws:iam::123456789012:user/airlock-stand-in</Arn>` +
			`<UserId>AIDAAIRLOCKSTANDIN00</UserId><Account>123456789012</Account></GetCallerIdentityResult>` +
			`<ResponseMetadata><RequestId>airlock-stand-in</RequestId></ResponseMetadata>` +
			`</GetCallerIdentityResponse>`, true
	case seen.Method == http.MethodGet && (seen.Path == "" || seen.Path == "/"):
		return header + `<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
			`<Owner><ID>airlock-stand-in</ID><DisplayName>airlock-stand-in</DisplayName></Owner>` +
			`<Buckets></Buckets></ListAllMyBucketsResult>`, true
	case seen.Method == http.MethodGet:
		return header + `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
			`<Name>` + strings.Trim(seen.Path, "/") + `</Name><Prefix></Prefix><KeyCount>0</KeyCount>` +
			`<MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated></ListBucketResult>`, true
	}
	return "", false
}

// Refusals is why each call this stand-in refused did not verify, in order.
func (a *AWSStand) Refusals() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.refusals...)
}

// sigV4Scheme is the signing algorithm's name, which an Authorization header opens with. It is
// the name AWS gives it and the one airlockproxy's aws.go spells out as well: a wire constant,
// not a setting, and the two cannot differ.
const sigV4Scheme = "AWS4-HMAC-SHA256"

// VerifySigV4 is one call's check against the real key: the Signature Version 4 specification
// itself - the canonical request, the string to sign, the derived key, the HMAC - carried out
// here rather than through an AWS library, and the refusal of any placeholder anywhere in the
// request. It answers nil for a call AWS would accept and what is wrong for every other.
func VerifySigV4(seen Request, keyID, secretKey, sessionToken string) error {
	if dump := fmt.Sprint(seen.Method, seen.Path, seen.Query, seen.Host, seen.Header, seen.Body); strings.Contains(dump, requests.PlaceholderPrefix) {
		return fmt.Errorf("this call carries a placeholder: %s", dump)
	}
	authorization := seen.Header.Get("Authorization")
	rest, isV4 := strings.CutPrefix(authorization, sigV4Scheme+" ")
	if !isV4 {
		return fmt.Errorf("the Authorization is no %s credential: %q", sigV4Scheme, authorization)
	}
	var credential, signedHeaders, signature string
	for _, part := range strings.Split(rest, ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch name {
		case "Credential":
			credential = value
		case "SignedHeaders":
			signedHeaders = value
		case "Signature":
			signature = value
		}
	}
	scope := strings.Split(credential, "/")
	if len(scope) != 5 {
		return fmt.Errorf("the Credential is no KEY/date/region/service/aws4_request scope: %q", credential)
	}
	if scope[0] != keyID {
		return fmt.Errorf("the Credential names the key id %q; want the real key id %q", scope[0], keyID)
	}
	if got := seen.Header.Get("X-Amz-Security-Token"); got != sessionToken {
		return fmt.Errorf("X-Amz-Security-Token = %q; want %q", got, sessionToken)
	}
	date, region, service := scope[1], scope[2], scope[3]
	payload := seen.Header.Get("X-Amz-Content-Sha256")
	if payload == "" {
		sum := sha256.Sum256([]byte(seen.Body))
		payload = hex.EncodeToString(sum[:])
	}
	var canonicalHeaders strings.Builder
	for _, name := range strings.Split(signedHeaders, ";") {
		var value string
		switch name {
		case "host":
			value = seen.Host
		case "content-length":
			if value = seen.Header.Get("Content-Length"); value == "" {
				value = strconv.Itoa(len(seen.Body))
			}
		default:
			value = strings.Join(seen.Header.Values(http.CanonicalHeaderKey(name)), ",")
		}
		canonicalHeaders.WriteString(name + ":" + strings.TrimSpace(value) + "\n")
	}
	query, err := url.ParseQuery(seen.Query)
	if err != nil {
		return fmt.Errorf("the query %q: %w", seen.Query, err)
	}
	for name := range query {
		slices.Sort(query[name])
	}
	canonical := strings.Join([]string{
		seen.Method,
		seen.Path,
		strings.ReplaceAll(query.Encode(), "+", "%20"),
		canonicalHeaders.String(),
		signedHeaders,
		payload,
	}, "\n")
	sum := sha256.Sum256([]byte(canonical))
	credentialScope := strings.Join([]string{date, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{sigV4Scheme, seen.Header.Get("X-Amz-Date"), credentialScope, hex.EncodeToString(sum[:])}, "\n")
	signing := hmacOf(hmacOf(hmacOf(hmacOf([]byte("AWS4"+secretKey), date), region), service), "aws4_request")
	if want := hex.EncodeToString(hmacOf(signing, stringToSign)); want != signature {
		return fmt.Errorf("the signature is %q; want %q, this call signed with the real key. canonical request:\n%s", signature, want, canonical)
	}
	return nil
}

func hmacOf(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// UpgradeOptions is the one route an UpgradeStand answers.
type UpgradeOptions struct {
	// Path is the path it answers an upgrade at, and "" is "/socket". One path and not every
	// path, because the host that serves a WebSocket serves an API beside it: every other
	// request is left to the service's own body, as the Microsoft stand leaves everything
	// that is not its authority's.
	Path string
}

// UpgradeStand is the WebSocket upgrade a bound host answers: 101 on its one path and then the
// little line protocol a caller passes frames with - one line in, and "the service read
// <that line>" back. It is what shows an upgrade crossing the airlock: the request is decided
// and swapped like any other, and once this has answered 101 the proxy joins the two
// connections and the bytes pass both ways until either side closes.
//
// Past the 101 it says nothing about an error: the connection ends when the caller walks away
// or the proxy's idle limit closes it, and each caller reads the frames - or their absence -
// from its own side.
type UpgradeStand struct {
	options UpgradeOptions
}

// NewUpgradeStand builds one. Install it with Service.AnswerWith(stand.Answer).
func NewUpgradeStand(o UpgradeOptions) *UpgradeStand { return &UpgradeStand{options: o} }

// Path is the path this stand answers an upgrade at.
func (u *UpgradeStand) Path() string {
	if u.options.Path == "" {
		return "/socket"
	}
	return u.options.Path
}

// Answer answers one upgrade, and false for every request on another path.
func (u *UpgradeStand) Answer(w http.ResponseWriter, r *http.Request, seen Request) bool {
	if seen.Path != u.Path() {
		return false
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "the stand-in service cannot be taken over", http.StatusInternalServerError)
		return true
	}
	if r.Header.Get("Upgrade") != "websocket" {
		http.Error(w, "the stand-in service was sent no Upgrade header", http.StatusBadRequest)
		return true
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "the stand-in service could not hijack: "+err.Error(), http.StatusInternalServerError)
		return true
	}
	defer conn.Close()
	if _, err := buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		return true
	}
	if err := buffered.Flush(); err != nil {
		return true
	}
	frame, err := buffered.ReadString('\n')
	if err != nil {
		return true
	}
	if _, err := buffered.WriteString("the service read " + strings.TrimSpace(frame) + "\n"); err != nil {
		return true
	}
	_ = buffered.Flush()
	return true
}

// ModelAnswer is one reply a ModelStand gives: the whole of the assistant's text, and how long
// the answer is held back before any of it is written.
//
// Text is the reply itself and no verdict of any kind: this package knows the Messages API's
// wire shape and nothing about what a caller asks a model for, so a structured reply is that
// JSON object as the reply's own text, which is how the protocol carries one and how agentloop
// reads a Run's Final value back out of it.
//
// Delay holds the whole answer back, the call's own context ending the wait: it is how a check
// spends a caller's time bound, since a delay past that bound is an answer the caller never
// sees at all.
type ModelAnswer struct {
	Text  string
	Delay time.Duration
}

// ModelOptions is what a ModelStand answers with.
type ModelOptions struct {
	// Answer is what it answers until Answers replaces it.
	Answer ModelAnswer
}

// ModelStand is a model endpoint speaking the Messages API, which is agentloop's
// anthropic-messages: it answers every call with one event stream carrying the answer it holds
// — a message of one text block that stopped at its end — and records each call on the Service,
// so a check reads back what was sent as well as what was answered.
//
// What it answers is set while it runs (Answers), because the cases a check drives against one
// model are its answers and not its calls: a structured reply the caller can read, one it
// cannot, and an answer that never comes.
type ModelStand struct {
	mu     sync.Mutex
	answer ModelAnswer
}

// NewModelStand builds one. Install it with Service.AnswerWith(stand.Answer).
func NewModelStand(o ModelOptions) *ModelStand { return &ModelStand{answer: o.Answer} }

// Answers replaces what every call from here on is answered with, which is how one run of a
// stack drives one case after another against a model that is already listening.
func (m *ModelStand) Answers(answer ModelAnswer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answer = answer
}

// standInModelID is the model this stand's own answer says wrote it. The caller's own model id
// rides in the call, which the Service records; nothing reads this one but a reader of the
// stream.
const standInModelID = "the-stand-in-model"

// standInModelUsage is the token count every answer reports. It is a plausible count and not a
// measured one — nothing here runs a tokenizer — so that a caller which records what a call
// spent has something of that shape to record.
var standInModelUsage = map[string]any{
	"input_tokens": 1200, "output_tokens": 48,
	"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
}

// Answer is one model call: the event stream the Messages API sends for a reply of one text
// block, written once the answer's delay has passed. A caller that walked away during that wait
// is answered nothing at all, which is the whole of what a stalled model is.
func (m *ModelStand) Answer(w http.ResponseWriter, r *http.Request, seen Request) bool {
	if seen.Method != http.MethodPost {
		http.Error(w, "the stand-in model endpoint takes POST, not "+seen.Method, http.StatusMethodNotAllowed)
		return true
	}
	m.mu.Lock()
	answer := m.answer
	m.mu.Unlock()
	if answer.Delay > 0 {
		select {
		case <-time.After(answer.Delay):
		case <-r.Context().Done():
			return true
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(name string, body map[string]any) {
		encoded, err := json.Marshal(body)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, encoded)
	}
	event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_standin", "type": "message", "role": "assistant", "model": standInModelID,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": standInModelUsage,
	}})
	event("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""}})
	event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": answer.Text}})
	event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": standInModelUsage})
	event("message_stop", map[string]any{"type": "message_stop"})
	return true
}

// DispatchIssue is one issue a DispatchStand holds.
type DispatchIssue struct {
	// Key is the issue's own key, the {key} both routes name it by.
	Key string
	// Title and Status are the other two fields the issue read answers of it. A reader of
	// these two routes treats the status done as work that is over, so a fixture holding one
	// such issue is how a check drives that case.
	Title, Status string
	// Spec is the markdown of the issue's spec document, and "" is an issue holding no such
	// document: the text route answers 404 for it, as Dispatch answers an issue with no spec.
	Spec string
}

// DispatchOptions is the fixture a DispatchStand answers from.
type DispatchOptions struct {
	// Issues are the issues it holds. A key none of them carries is 404 on both routes.
	Issues []DispatchIssue
}

// DispatchStand is the two reads the broker's model judge makes of Dispatch: GET
// /api/v1/issues/{key}, which answers the issue, and GET
// /api/v1/issues/{key}/artifacts/spec/text, which answers that issue's spec document. It
// answers both from a fixture, since a Dispatch of its own would want Postgres and a sign-in,
// and it keeps the one thing of the real API a fixture could quietly lose: a request carrying
// no bearer is refused 401 before either route is read (api.requireAuthenticated), so a caller
// that presented no token reads that refusal rather than an issue.
//
// Each answer carries the fields of the real one that a reader of these two routes reads —
// key, title and status of api.getIssue's issue, and the markdown of api.getArtifactText's
// document — and none of the artifacts, children, open asks and progress the issue read also
// answers: a caller that needs one of those is reading a route this stand does not stand in
// for.
type DispatchStand struct {
	issues map[string]DispatchIssue
}

// NewDispatchStand builds one. Install it with Service.AnswerWith(stand.Answer).
func NewDispatchStand(o DispatchOptions) *DispatchStand {
	issues := make(map[string]DispatchIssue, len(o.Issues))
	for _, issue := range o.Issues {
		issues[issue.Key] = issue
	}
	return &DispatchStand{issues: issues}
}

// Answer is Dispatch's answer to the two reads, and false for every other request, which leaves
// the service its own body: a caller that asked for a route this stand does not serve has not
// asked Dispatch anything.
func (d *DispatchStand) Answer(w http.ResponseWriter, _ *http.Request, seen Request) bool {
	key, spec, ok := dispatchRoute(seen)
	if !ok {
		return false
	}
	token, bearer := strings.CutPrefix(seen.Header.Get("Authorization"), "Bearer ")
	if !bearer || token == "" {
		dispatchError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "the stand-in Dispatch takes a bearer token")
		return true
	}
	issue, held := d.issues[key]
	switch {
	case !held:
		dispatchError(w, http.StatusNotFound, "NOT_FOUND", "the stand-in Dispatch holds no issue "+key)
	case spec && issue.Spec == "":
		dispatchError(w, http.StatusNotFound, "ARTIFACT_NOT_FOUND", "issue "+key+" holds no spec document")
	case spec:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"markdown": issue.Spec, "version": 1, "token": "sha256:the-stand-in-document-token",
		})
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"key": issue.Key, "title": issue.Title, "status": issue.Status,
		})
	}
	return true
}

// dispatchRoute reads which of the two routes a request is and the issue key it names: the
// issue read itself, and the text of that issue's spec document. Both are GETs, and every other
// request is no route of this stand's.
func dispatchRoute(seen Request) (key string, spec, ok bool) {
	if seen.Method != http.MethodGet {
		return "", false, false
	}
	rest, found := strings.CutPrefix(seen.Path, "/api/v1/issues/")
	if !found {
		return "", false, false
	}
	if key, found := strings.CutSuffix(rest, "/artifacts/spec/text"); found {
		return key, true, key != "" && !strings.Contains(key, "/")
	}
	return rest, false, rest != "" && !strings.Contains(rest, "/")
}

// dispatchError is a refusal in the shape the API writes one (api.writeError), so a caller that
// reads the code out of a refusal reads the same field here.
func dispatchError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message, "code": code})
}

// MetadataOptions is what a MetadataStand answers with.
type MetadataOptions struct {
	// Token is the identity token every ask is answered with, and "" is a stand-in token of
	// its own.
	Token string
}

// MetadataStand is the Compute Engine metadata server a machine reads its own Google identity
// token from (internal/googleidentity): GET
// /computeMetadata/v1/instance/service-accounts/default/identity, answered with the token as
// the whole of the body, and answered only for a request carrying Metadata-Flavor: Google,
// which the real server refuses a request without. The audience asked for rides in the query,
// which the Service records, so a check reads back which audience a caller minted for.
//
// That is the whole of what that reader asks of a metadata server, so pointing GCE_METADATA_HOST
// at one of these is what lets a local stack run the production credential path itself rather
// than a test-only setting standing beside it.
type MetadataStand struct {
	options MetadataOptions
}

// NewMetadataStand builds one. Install it with Service.AnswerWith(stand.Answer).
func NewMetadataStand(o MetadataOptions) *MetadataStand { return &MetadataStand{options: o} }

// metadataIdentityPath is where the instance's default service account's identity token is
// minted, the path internal/googleidentity asks for.
const metadataIdentityPath = "/computeMetadata/v1/instance/service-accounts/default/identity"

// Answer is the metadata server's answer to one identity token, and false for every other
// request, which leaves the service its own body.
func (m *MetadataStand) Answer(w http.ResponseWriter, _ *http.Request, seen Request) bool {
	if seen.Path != metadataIdentityPath {
		return false
	}
	if seen.Header.Get("Metadata-Flavor") != "Google" {
		http.Error(w, "the stand-in metadata server takes the header Metadata-Flavor: Google", http.StatusForbidden)
		return true
	}
	token := m.options.Token
	if token == "" {
		token = "the-stand-in-google-identity-token"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, token)
	return true
}
