package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/machine"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// deviceAuthorization is POST /v1/oauth/device_authorization: a machine login's device
// authorization request (RFC 8628), public. It spends its source address's launcher bucket before
// the provider does any work, as the token login does (refuseAddress). At the edge, every login
// it opens spends exactly one token of the edge's one bucket every caller shares
// (launcherLimiter.overall): a request the pre-check judges able to open a login
// (deviceLoginCanOpen) spends it before the provider does any work, and a login
// Deps.DeviceLogin opens that the pre-check did not judge so, as one whose iat crossed a second
// between the two clock reads does, spends it once opened, a refusal rolling the login back. A
// request that opens no login spends nothing of it. Deps.DeviceLogin verifies the request and
// opens the login in the provider's transaction, and only a login it opened spends the bucket of
// the person its login_hint names (spendDeviceLoginBucket), so a request that writes nothing costs
// that person nothing; a refusal there rolls the login back too. A refused request is 429 with
// Retry-After. Deps.DeviceLogin's refusals are OAuth errors (invalid_request) whose description
// names the broker's code: a request object that does not verify, repeats a jti or names another
// key than dpop_jkt REQUEST_INVALID, one naming a service SERVICE_LOGS_IN_WITH_TOKEN. The answer's
// verification_uri is this listener's page (server.verificationURI).
func (s *server) deviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if !s.launcherLimiter.addressAllows(r) {
		oauth.WriteRefusal(w, rateLimitedRefusal(s.launcherLimiter.perAddress.Every()))
		return
	}
	if !readOAuthForm(w, r) {
		return
	}
	spent := false
	if s.launcherLimiter.overall != nil && s.deviceLoginCanOpen(r.PostForm) {
		if !s.launcherLimiter.overallAllows() {
			oauth.WriteRefusal(w, rateLimitedRefusal(s.launcherLimiter.overall.Every()))
			return
		}
		spent = true
	}
	s.deps.OAuth.DeviceAuthorize(w, r, s.verificationURI, func(ctx context.Context, form url.Values, userCode string) error {
		if err := s.deps.DeviceLogin(ctx, form, userCode); err != nil {
			return deviceLoginRefusal(err)
		}
		if !spent && !s.launcherLimiter.overallAllows() {
			return deviceLoginRefusal(rateLimitedRefusal(s.launcherLimiter.overall.Every()))
		}
		return deviceLoginRefusal(s.spendDeviceLoginBucket(r, form))
	})
}

// deviceLoginCanOpen reports whether form is a device authorization the provider can open a login
// for: the device login's own client, and a person's machine login Deps.DeviceLogin would open
// short of its jti (machine.Service.CheckPersonLogin: the object's signature, audience, times,
// detail and reason, no service, a person's login_hint and a dpop_jkt that is its key), none of
// which reads the database. A request that fails either is refused by the provider or by
// Deps.DeviceLogin with nothing written. A replay of an object that passes spends the edge's shared
// bucket, since only Deps.DeviceLogin reads its jti; that is the accepted cost of checking without
// the database. The judgment reads its own clock, so a login it refuses can still open; such a
// login spends the bucket once open (deviceAuthorization).
func (s *server) deviceLoginCanOpen(form url.Values) bool {
	if form.Get("client_id") != oauthwire.ClientID {
		return false
	}
	return s.deps.MachineLogin.CheckPersonLogin(form.Get("request"), form.Get("dpop_jkt"), time.Now()) == nil
}

// spendDeviceLoginBucket spends one request of the bucket of the person form's request object
// names, which Deps.DeviceLogin has verified (deviceLoginKey), and is a RATE_LIMITED refusal when
// that bucket is empty.
func (s *server) spendDeviceLoginBucket(r *http.Request, form url.Values) error {
	obj, err := record.VerifyRequestObject(form.Get("request"), s.deps.MachineLogin.Audience, s.deps.MachineLogin.Skew, time.Now())
	if err != nil {
		return err
	}
	if !s.launcherLimiter.perLogin.AllowAt(s.deviceLoginKey(obj, r), time.Now()) {
		return rateLimitedRefusal(s.launcherLimiter.perLogin.Every())
	}
	return nil
}

// deviceLoginKey is the per-login bucket a device login of obj from r spends, under this
// listener's key prefix (server.loginKeyPrefix): person:<login> on the broker's own address
// (launcherLoginKey), and at the edge the person at the caller's address (edgePersonKey,
// server.loginKeyByAddress), so a flood from one address spends that address's bucket for the
// person and never the one a caller elsewhere uses.
func (s *server) deviceLoginKey(obj record.RequestObject, r *http.Request) string {
	if !s.loginKeyByAddress {
		return s.loginKeyPrefix + launcherLoginKey(obj)
	}
	return s.loginKeyPrefix + edgePersonKey(record.CanonicalLogin(obj.LoginHint), s.address(r))
}

// edgePersonKey is the key of a person's bucket at an address: person:<length of
// login>:<login>:<address>. Both a login (a quoted local part) and an address (IPv6) can hold
// ":", so the login's length comes first and no two pairs make one key.
func edgePersonKey(login, address string) string {
	return "person:" + strconv.Itoa(len(login)) + ":" + login + ":" + address
}

// deviceLoginRefusal is the OAuth refusal a device login's error answers with: the broker's own
// status and code for it. An error already a refusal, and one the broker has no code for, pass
// through (the provider answers the second 500 server_error).
func deviceLoginRefusal(err error) error {
	var refusal *oauth.Refusal
	switch {
	case err == nil, errors.As(err, &refusal):
		return err
	case errors.Is(err, machine.ErrServiceLogsInWithToken):
		return &oauth.Refusal{Status: http.StatusBadRequest, Code: "SERVICE_LOGS_IN_WITH_TOKEN", Message: err.Error()}
	case errors.Is(err, record.ErrRequestInvalid):
		return &oauth.Refusal{Status: http.StatusBadRequest, Code: "REQUEST_INVALID", Message: err.Error()}
	default:
		return err
	}
}

// rateLimitedRefusal is a device login's 429 RATE_LIMITED refusal from a bucket refilling one
// request every every.
func rateLimitedRefusal(every time.Duration) *oauth.Refusal {
	return &oauth.Refusal{
		Status: http.StatusTooManyRequests, Code: "RATE_LIMITED",
		Message:    "too many launcher credential requests; try again later",
		RetryAfter: ratelimit.RetryAfterSeconds(every),
	}
}

// oauthToken is POST /v1/oauth/token: a machine login's token endpoint, public. It answers the
// device code grant once the login's person has approved it, and the refresh grant, each with a
// DPoP proof from the machine's key, and its answers are OAuth's own (RFC 6749 section 5). The
// provider checks the proof's htu against the URL it rebuilds from the request's scheme, Host and
// X-Forwarded-Proto, so the request it is handed carries Deps.PublicURL's scheme and host and
// no forwarding headers: the proof names PublicURL + /v1/oauth/token, as every other broker proof
// names PublicURL + its path, whatever the hop in front of the broker sends. At the broker's edge
// that URL is BROKER_EDGE_URL (RegisterEdge), so a proof made for either listener is refused at
// the other. It first spends its source address's token bucket (DefaultTokenLimit), a refusal
// being 429 invalid_request RATE_LIMITED with Retry-After, as the device authorization
// endpoint's is.
func (s *server) oauthToken(w http.ResponseWriter, r *http.Request) {
	if !s.tokenAllows(r) {
		oauth.WriteRefusal(w, &oauth.Refusal{
			Status: http.StatusTooManyRequests, Code: "RATE_LIMITED",
			Message:    "too many token requests; try again later",
			RetryAfter: ratelimit.RetryAfterSeconds(s.tokenLimiter.Every()),
		})
		return
	}
	if !readOAuthForm(w, r) {
		return
	}
	s.deps.OAuth.Token(w, s.atPublicURL(r), s.deps.TokenHooks)
}

// maxOAuthFormBytes caps an OAuth route's form body: a device authorization or token request is
// a few KiB, and the provider stores the whole form.
const maxOAuthFormBytes = 64 << 10

// readOAuthForm parses r's form under maxOAuthFormBytes before the provider sees it, so the
// provider reads the parsed form rather than a body of up to its library's 10 MiB. A body past the
// cap, or a form that does not parse, is refused 400 invalid_request REQUEST_INVALID. It reports
// whether the form parsed.
func readOAuthForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxOAuthFormBytes)
	if err := r.ParseForm(); err != nil {
		oauth.WriteRefusal(w, &oauth.Refusal{Status: http.StatusBadRequest, Code: "REQUEST_INVALID",
			Message: fmt.Sprintf("the form must parse and be at most %d bytes: %v", maxOAuthFormBytes, err)})
		return false
	}
	return true
}

// atPublicURL is r as if it had reached BROKER_PUBLIC_URL directly: its URL's scheme and host and
// its Host are the public URL's, it carries TLS state exactly when that URL is https, and it has
// no X-Forwarded-Proto or X-Forwarded-Host.
func (s *server) atPublicURL(r *http.Request) *http.Request {
	public, err := url.Parse(s.deps.PublicURL)
	if err != nil {
		// config.Load refuses a BROKER_PUBLIC_URL that does not parse.
		panic("api: PublicURL does not parse: " + err.Error())
	}
	out := r.Clone(r.Context())
	out.URL.Scheme, out.URL.Host, out.Host = public.Scheme, public.Host, public.Host
	out.TLS = nil
	if public.Scheme == "https" {
		out.TLS = &tls.ConnectionState{}
	}
	out.Header.Del("X-Forwarded-Proto")
	out.Header.Del("X-Forwarded-Host")
	return out
}
