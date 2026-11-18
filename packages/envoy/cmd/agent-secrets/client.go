// packages/envoy/cmd/agent-secrets/client.go
//
// client is agent-secrets's own thin HTTP client for the broker routes it needs, as the broker's
// HTTP API reference (https://cybermonklabs.github.io/legion/broker/reference/api/) defines them. Session routes
// — everything but enrollment issuance/revocation and launcher-credential issuance — are signed
// per call through a Signer (proofsource.go: an agent box or pod's own key, or a machine session's
// agent-secrets-helper), producing exactly the Proof header internal/broker/proof.Verifier
// expects. CreateRequest additionally needs a signed credential-request object (record.Sign)
// embedded in its body, since the broker carries authorization_details and
// reason inside that signed object rather than as plain top-level JSON fields; building it is
// also a Signer responsibility (Signer.SignRequestObject), since a machine session's key never
// leaves agent-secrets-helper.
// Enrollment issuance/revocation and launcher-credential issuance are helper-socket operations
// (see cmdEnrollHelper/cmdUnenrollHelper in enroll.go, cmdMachineLogin in machine.go, and
// internal/broker/helper): no route here ever carries a bearer launcher token, and the helper's
// machine login (an OAuth device grant) carries no launcher credential (a launcher has none yet). The
// operator routes (/v1/operator/*) are signed with a launcher proof the helper's sign-launcher op
// makes (launcherSigner). Every method that a subcommand may need to print verbatim under --json
// (request, status, self, the operator lists) returns both its decoded result and the exact raw
// response bytes, so no command re-marshals a Go struct in place of the wire body.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/leasetime"
	"github.com/sjawhar/envoy/internal/redirects"
)

type client struct {
	baseURL brokeraddr.Address
	http    *http.Client
}

// terminalRenewal is the closed set of renewal refusals `agent-secrets renew` stops on, exiting
// exitRenewRefused (76) so the worker shim does not restart it; every other refusal keeps the retry
// backoff. Each member is an answer no retry can change: POD_TOKEN_REQUIRED is a pod enrollment's
// renewal that carried no token, and
// ENROLLMENT_NOT_LIVE an enrollment that has ended, lapsed or never existed. A broker that predates
// ENROLLMENT_NOT_LIVE answers an ended enrollment
// PROOF_INVALID with a message saying "not live" (its renew handler's "enrollment is not live" and
// its proof check's "proof invalid: not live"), which no recoverable proof failure says.
// POD_IDENTITY_MISMATCH is not a member: the broker also answers it for a token it could not
// verify while its issuer or key set was unreachable, and a real mismatch ends with the lease,
// after which the next renewal is ENROLLMENT_NOT_LIVE.
var terminalRenewal = struct {
	Codes                     []string
	LegacyCode, LegacyMessage string
}{
	Codes:         []string{"POD_TOKEN_REQUIRED", enrollmentEndedCode},
	LegacyCode:    "PROOF_INVALID",
	LegacyMessage: "not live",
}

// enrollmentEndedCode is the renew route's own answer to a proof naming an enrollment the broker
// no longer holds, written once here and read by terminalRenewal and by enrollmentEnded.
const enrollmentEndedCode = "ENROLLMENT_NOT_LIVE"

// newClient is the HTTP client every agent-secrets call goes out on, renewals among them. Its
// whole-call timeout is leasetime.RenewClientTimeout, the same figure the broker's lease bound
// charges a renewal an outage black-holes: a connection a front door accepts with nothing
// behind it costs this renewer the whole of it.
//
// Redirects are refused (redirects.Refused): every call this client makes carries a Proof header
// of this session's own (doProof), and a 3xx answered for the broker - by the broker, or by
// anyone who can answer for it - must not hand that proof to a host the deployment never named.
func newClient(baseURL brokeraddr.Address) *client {
	return &client{baseURL: baseURL, http: redirects.Refused(&http.Client{Timeout: leasetime.RenewClientTimeout})}
}

// route is path's URL at the broker, built from the address as configured (Raw) to be sent and
// signed, never printed: a message names it through c.baseURL.Call.
func (c *client) route(path string) string { return c.baseURL.Raw() + path }

// apiError is the broker's {"code":...,"error":...,"answer_id":...} error envelope, plus the HTTP
// status it came with. AnswerID is the id the broker's own record of the answer names: the body's
// answer_id, else the X-Broker-Answer-Id header every answer the broker itself writes carries, so
// a refusal whose body is not the envelope (the broker's 404 for a path it does not serve) still
// names it. A broker from before the id sends neither, and neither does an answer net/http writes
// before any of the broker's code runs, such as its 400 for a malformed request, 431 and 417.
//
// Code is the answer's code as it came, which the refusals this client tells apart compare, and
// said its words as they came, which they read (says): nothing prints said, a *string, as
// brokeraddr.Address's address is, so fmt prints the pointer, never the text, where it prints this
// struct field by field (%#v). Message and AnswerID are the words and the answer id as a message
// may print them (brokeraddr.Address.Words): beside an address that is not clean, whoever answers
// can repeat the credential the address carries in any text it answers, so there Message is
// brokeraddr.WordsNotPrinted, withheld is set where doStatus builds the refusal, and the refusal
// prints as its status and Message alone, with neither its code nor its answer id, as the token
// client prints such an answer (internal/brokerclient). The one line that names a code whatever
// the address, renew's exit on a terminal refusal, names one of terminalRenewal's, which this
// client matched it against.
type apiError struct {
	Status   int
	Code     string
	Message  string
	AnswerID string
	said     *string
	// withheld is whether the answer came from an address that is not clean, so that none of
	// its text prints: the decision doStatus made, read by Error and podRefusal.
	withheld bool
}

// Error is the broker's message and code, and the answer id to quote when it named one; where
// the answer's text is withheld, the status and Message alone. A body that was not the envelope is
// the message as it came, a line ending and all (the mux's "404 page not found\n"), so the message
// is cut at its trailing line endings before the id goes after it on the same line.
func (e *apiError) Error() string {
	if e.withheld {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	message := strings.TrimRight(e.Message, "\r\n")
	var text string
	if e.Code == "" {
		text = fmt.Sprintf("HTTP %d: %s", e.Status, message)
	} else {
		text = fmt.Sprintf("%s (%s)", message, e.Code)
	}
	if e.AnswerID != "" {
		text += " (answer " + e.AnswerID + ")"
	}
	return text
}

// says reports whether the broker's error text holds phrase. A refusal this client made itself
// (a redirect) holds no broker text, and says nothing.
func (e *apiError) says(phrase string) bool {
	return e.said != nil && strings.Contains(*e.said, phrase)
}

// enrollmentEnded reports whether this refusal is the broker's answer to a proof naming an
// enrollment it no longer holds: the lease lapsed, or it was revoked. Only the renew route
// answers ENROLLMENT_NOT_LIVE; every other session route says PROOF_INVALID with "not live" in
// its message, which no recoverable proof failure (iat skew, key, URL) says.
func (e *apiError) enrollmentEnded() bool {
	return e.Code == enrollmentEndedCode ||
		(e.Code == terminalRenewal.LegacyCode && e.says(terminalRenewal.LegacyMessage))
}

// Permanent reports whether this refusal is one of terminalRenewal's: a code it lists, or the
// ended enrollment its legacy code spells (enrollmentEnded).
func (e *apiError) Permanent() bool {
	return slices.Contains(terminalRenewal.Codes, e.Code) || e.enrollmentEnded()
}

// unreachableError is a broker call that did not complete. Its host might not resolve, nothing
// might answer at the address, or a peer might answer the connection but fail the exchange. It is
// not a broker refusal - the broker sent no usable response - and it reads as what it is, naming
// the address tried and the variable the address came from, because a session that inherited a
// stale AGENT_SECRETS_URL otherwise reads a bare DNS error as a broker that is down and the stale
// value goes unnoticed.
type unreachableError struct {
	// what classifies the failure: a failed name resolution, no connection made, nothing answered
	// in time, a peer that answered but failed the exchange, or a request that failed in a way
	// none of those names.
	what string
	// base is the broker's address as AGENT_SECRETS_URL gave it, method and url the call tried;
	// Error prints base as the Address prints and url through base.Call, so a credential the
	// variable carries never prints, while an id the call path-escaped still does.
	base        brokeraddr.Address
	method, url string
	// cause is the transport's own error through base.Cause: without net/http's *url.Error
	// wrapper, which repeats the URL, or its class alone where its text may not print.
	cause error
}

func (e *unreachableError) Error() string {
	return fmt.Sprintf("%s: %s %s (%s names %s): %v", e.what, e.method, e.base.Call(e.url), brokerURLVar, e.base, e.cause)
}

func (e *unreachableError) Unwrap() error { return e.cause }

// unreachable classifies a failed HTTP exchange, sending its reader to the layer that failed.
// Any resolver error is name resolution, before the generic timeout branch. A dial that made no
// connection (refused, network or host unreachable, any other dial error) is a non-answer, which
// routing or a listener fixes. Only what proves a peer answered - a TLS record or certificate
// error, EOF, or a reset once the connection existed - is "answered but the exchange failed",
// which TLS or a proxy fixes. An error that fits none says no more than that the request failed,
// rather than name a layer it cannot prove.
func unreachable(base brokeraddr.Address, method, callURL string, err error) error {
	cause := base.Cause(err)
	host := brokerHost(base)
	failed := func(what string) error { return &unreachableError{what, base, method, callURL, cause} }
	var dns *net.DNSError
	var timeout net.Error
	var op *net.OpError
	switch {
	case errors.As(err, &dns) && dns.IsNotFound:
		return failed("the broker's host " + host + " does not resolve")
	case errors.As(err, &dns):
		return failed("the broker's host " + host + " name resolution failed")
	case errors.Is(err, syscall.ECONNREFUSED):
		return failed("nothing answered at " + host + ": the connection was refused")
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()):
		return failed("the broker at " + host + " did not answer in time")
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH),
		errors.As(err, &op) && op.Op == "dial":
		return failed("nothing answered at " + host + ": no connection could be made")
	case peerAnswered(err):
		return failed("the broker at " + host + " answered but the exchange failed")
	default:
		return failed("the request to the broker at " + host + " failed")
	}
}

// peerAnswered reports whether err shows a peer took the connection and then broke the exchange:
// a TLS record that is not TLS (an HTTP server answering an https URL, which net/http reports as
// its fixed http.ErrSchemeMismatch in place of the record error), a TLS alert or certificate it
// would not trust, or the stream ending or being reset once it existed.
func peerAnswered(err error) bool {
	var record tls.RecordHeaderError
	var alert tls.AlertError
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &record) || errors.Is(err, http.ErrSchemeMismatch) || errors.As(err, &alert) || errors.As(err, &verify) ||
		errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// cutOffError is a broker answer lost after its headers came: the status line arrived and the
// body did not, because the stream broke while doStatus read it. Its text is the call, then
// "read response:", then the transport's own cause.
type cutOffError struct {
	call  string
	cause error
}

func (e *cutOffError) Error() string { return e.call + ": read response: " + e.cause.Error() }

func (e *cutOffError) Unwrap() error { return e.cause }

// retryable reports whether a later call may get the answer err's call did not: the call went
// out and failed in transport (unreachable's classes), its answer was cut off after the headers
// came (*cutOffError), or the broker, or a front door before it, could not serve it just then —
// 429, 502, 503 or 504, with the broker's envelope or without it (doStatus makes an *apiError of
// either). Any other refusal is the broker's answer, which the same call gets again, and a
// failure of the local signer (the helper's socket) is not the broker's at all.
func retryable(err error) bool {
	var unreached *unreachableError
	if errors.As(err, &unreached) {
		return !errors.Is(err, errNothingSent)
	}
	var refusal *apiError
	if errors.As(err, &refusal) {
		return slices.Contains([]int{
			http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		}, refusal.Status)
	}
	var cut *cutOffError
	return errors.As(err, &cut)
}

// do sends the request and, on a 2xx response, returns its raw body; any other status decodes
// the broker's error envelope into an *apiError.
func (c *client) do(ctx context.Context, method, url string, body []byte, headers map[string]string) ([]byte, error) {
	_, data, err := c.doStatus(ctx, method, url, body, headers)
	return data, err
}

// doStatus is do, also answering the 2xx status itself, which tells apart the two answers a
// route gives for success — POST /v1/enrollments's 201 for a new enrollment and 200 for the one
// this key already holds.
func (c *client) doStatus(ctx context.Context, method, url string, body []byte, headers map[string]string) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		// With a base that prints as written, the part that did not parse is the path this
		// client built, so the error says so (through Error, which prints it as net/url wrote it
		// only where that is clean). With any other base, the parser's own text could quote a bad
		// escape in a password, so the message says only that the address is not valid.
		if c.baseURL.Clean() {
			return 0, nil, fmt.Errorf("the request could not be built: %w", c.baseURL.Error(err))
		}
		return 0, nil, &unreachableError{"the broker's address is not a valid URL", c.baseURL, method, url, errNothingSent}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, unreachable(c.baseURL, method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, &cutOffError{method + " " + c.baseURL.Call(url), c.baseURL.Error(err)}
	}
	if resp.StatusCode/100 == 3 {
		// A redirect this client refuses to follow (redirects.Refused), handed back as the
		// non-2xx it is. Its body is not printed: net/http writes the target into the body
		// of a redirect it answers a GET with (`<a href="…">Found</a>`), and that target,
		// with whatever query it carries, is what refusing the redirect keeps out of this
		// process. The status alone says what happened.
		return resp.StatusCode, nil, &apiError{Status: resp.StatusCode, Message: "a redirect, which this client does not follow"}
	}
	if resp.StatusCode/100 != 2 {
		var envelope struct {
			Code     string `json:"code"`
			Error    string `json:"error"`
			AnswerID string `json:"answer_id"`
		}
		_ = json.Unmarshal(data, &envelope)
		if envelope.Error == "" {
			envelope.Error = string(data)
		}
		if envelope.AnswerID == "" {
			envelope.AnswerID = resp.Header.Get(answerIDHeader)
		}
		// Beside an address that is not clean, whoever answers can repeat its credential in
		// any text of its answer: the refusal prints as its status alone (apiError.withheld).
		return resp.StatusCode, nil, &apiError{
			Status:   resp.StatusCode,
			Code:     envelope.Code,
			Message:  c.baseURL.Words(envelope.Error),
			AnswerID: c.baseURL.Words(envelope.AnswerID),
			said:     &envelope.Error,
			withheld: !c.baseURL.Clean(),
		}
	}
	return resp.StatusCode, data, nil
}

// answerIDHeader is the header every answer the broker itself writes names its answer id in
// (answer.Header in internal/broker/answer, which this client does not link); net/http's own
// 400, 431 and 417, written before any of the broker's code runs, carry none.
const answerIDHeader = "X-Broker-Answer-Id"

// errNothingSent is the cause of a call this client could not even build a request for.
var errNothingSent = errors.New("nothing was sent")

// reenroller is a signer that can replace an enrollment the broker no longer holds: a person's
// sign-in on this box, and a machine that registered its own session here (proofsource.go).
// Nothing else is one — a box or pod its launcher enrolled is the launcher's to enrol again,
// and a launcher proof names no enrollment at all.
type reenroller interface {
	Reenroll(ctx context.Context) error
}

// enrollmentNotLive reports whether err is a refusal (*apiError) naming an enrollment the broker
// no longer holds, which is enrollmentEnded's question asked of an error.
func enrollmentNotLive(err error) bool {
	var refusal *apiError
	return errors.As(err, &refusal) && refusal.enrollmentEnded()
}

// doProof signs the request with signer.Sign against exactly this method and URL (no query
// string appended after signing), the shape proof.Verifier.Verify compares htm/htu against.
func (c *client) doProof(ctx context.Context, signer proofSigner, method, path string, body []byte) ([]byte, error) {
	_, data, err := c.doProofStatus(ctx, signer, method, path, body)
	return data, err
}

// doProofStatus is doProof, also answering the 2xx status (doStatus).
//
// A signer that can register itself again (reenroller) gets exactly one retry when the broker
// says the enrollment its proof named is not live. Nothing renews a sign-in's or a machine's own
// session enrollment otherwise, so after BROKER_LEASE_SECONDS every call on that box would fail
// with the dead id still in its record, and only signing in again would cure it — making a
// second box key and a second sign-in on the person's list. One retry, because a second refusal
// is the broker saying no to the enrollment this call just made, which no further retry changes.
func (c *client) doProofStatus(ctx context.Context, signer proofSigner, method, path string, body []byte) (int, []byte, error) {
	return c.doProofBuilt(ctx, signer, method, path, func(bool) ([]byte, error) { return body, nil })
}

// doProofBuilt is doProofStatus for a body the session's own key signs part of, which build
// makes: build(false) for the first call, and build(true) again for the retry after the session
// registered again, since a machine registers again under a key of its own (machineReregister)
// and the broker takes only what the enrollment's own key signed.
func (c *client) doProofBuilt(ctx context.Context, signer proofSigner, method, path string, build func(reregistered bool) ([]byte, error)) (int, []byte, error) {
	body, err := build(false)
	if err != nil {
		return 0, nil, err
	}
	status, data, err := c.proofCall(ctx, signer, method, path, body)
	again, ok := signer.(reenroller)
	if !ok || !enrollmentNotLive(err) {
		return status, data, err
	}
	if reErr := again.Reenroll(ctx); reErr != nil {
		return status, data, fmt.Errorf("%w; registering this session again: %v", err, reErr)
	}
	if body, err = build(true); err != nil {
		return 0, nil, err
	}
	return c.proofCall(ctx, signer, method, path, body)
}

// proofCall is one signed call, which is all `renew` may make: its whole contract is to report
// the enrollment the broker no longer holds (terminalRenewal, exit 76) rather than quietly
// register another.
func (c *client) proofCall(ctx context.Context, signer proofSigner, method, path string, body []byte) (int, []byte, error) {
	url := c.route(path)
	token, err := signer.Sign(method, url)
	if err != nil {
		return 0, nil, fmt.Errorf("sign proof: %w", err)
	}
	return c.doStatus(ctx, method, url, body, map[string]string{"Proof": token})
}

// --- POST /v1/requests, GET /v1/requests/{id}, POST /v1/requests/{id}/cancel ---

// SecretDecision mirrors requests.SecretDecision's wire shape.
type SecretDecision struct {
	Name     string `json:"name"`
	Decision string `json:"decision"`
}

// RequestResult is POST /v1/requests's exact response shape. LifetimeSeconds is the lifetime the
// broker gave the request's grant, nil for until its session ends; a broker from before
// lifetime_seconds sends no such field, and it decodes as nil. ApproveURL is the record's page on
// the broker, where its approver signs in and decides it, while the request is pending; empty
// once it is not, and from a broker from before that page. Detail is the broker's one sentence
// about why it denied the request — the secret that refused it and the rule that did, as
// "GCLOUD_SAMI: requester-not-listed" — which every line this CLI prints about a refusal carries,
// so a session reads that asking again will not help; nil when the broker recorded none, and from
// a broker from before the field.
type RequestResult struct {
	RequestID       string           `json:"request_id"`
	State           string           `json:"state"`
	Secrets         []SecretDecision `json:"secrets"`
	GrantID         *string          `json:"grant_id"`
	RecordID        *string          `json:"record_id"`
	Approver        *string          `json:"approver"`
	LifetimeSeconds *int             `json:"lifetime_seconds"`
	Coalesced       bool             `json:"coalesced,omitempty"`
	Detail          *string          `json:"detail"`
	ApproveURL      string           `json:"approve_url"`
}

// createRequestBody is POST /v1/requests's exact shape in the broker's HTTP API: a signed
// request object plus two optional, unsigned fields, session_id (wake-only) and github_app, the
// narrowing this request's grant will be released with. The v8 top-level
// "secrets"/"reason"/"issue" fields are gone — they live inside the signed request object instead.
// github_app is left out when the run asks for no narrowing, so such a run sends the body it
// always sent; it narrows nothing by itself, the release being what applies a narrowing, and the
// broker checks it before it asks a person to approve the request.
type createRequestBody struct {
	Request   string           `json:"request"`
	SessionID *string          `json:"session_id"`
	GitHubApp *GitHubNarrowing `json:"github_app,omitempty"`
}

// brokerTookNoNarrowing reports whether err is a broker from before POST /v1/requests took a
// github_app field: its readJSON refuses an unknown field with 400 INVALID_REQUEST, and refuses
// it before the request machine runs, so nothing was written and the signed request object's jti
// is unspent. Every refusal of the narrowing itself has a code of its own (INVALID_NARROWING,
// NARROWING_NOT_APPLICABLE, NARROWING_OWNER_NOT_LISTED, NARROWING_BEHIND_AIRLOCK,
// NARROWING_REFUSED), and a request object the broker would not take is REQUEST_INVALID, so none
// of them reads as an older broker and none is sent again.
func brokerTookNoNarrowing(err error) bool {
	var refusal *apiError
	return errors.As(err, &refusal) && refusal.Status == http.StatusBadRequest && refusal.Code == "INVALID_REQUEST"
}

// CreateRequest builds and signs a credential-request object carrying one
// agent_secret authorization_detail per requested name and lifetimeSeconds, the grant's lifetime
// (nil for until the session ends) (Signer.SignRequestObject, audience == the broker's own base
// URL), then calls POST /v1/requests with that object nested in the body and the outer call still
// authenticated with a session Proof header. It returns both the decoded result and the exact raw
// response bytes, for --json's "print the response body verbatim" requirement.
//
// narrowing is the narrowing the release will ask for, sent as github_app so the broker refuses a
// narrowing it cannot serve before anybody is asked to approve the request; nil sends no such
// field. A broker from before that field refuses the whole body (brokerTookNoNarrowing) without
// reading the request object, so the same signed object is sent once more without the field and
// that second call's own result and raw bytes are what this answers. Once only: a second refusal
// is about something else, which sending it again cannot mend. When the session registers again
// on the way (doProofBuilt), the object is signed again, by the key the session now holds.
func (c *client) CreateRequest(ctx context.Context, signer Signer, names []string, reason string, lifetimeSeconds *int, sessionID string, narrowing *GitHubNarrowing) (RequestResult, []byte, error) {
	sign := func() (string, error) {
		requestObject, err := signer.SignRequestObject(c.baseURL, names, reason, lifetimeSeconds)
		if err != nil {
			return "", fmt.Errorf("sign request object: %w", err)
		}
		return requestObject, nil
	}
	requestObject, err := sign()
	if err != nil {
		return RequestResult{}, nil, err
	}
	var sessionIDPtr *string
	if sessionID != "" {
		sessionIDPtr = &sessionID
	}
	body := func(narrowing *GitHubNarrowing) func(bool) ([]byte, error) {
		return func(reregistered bool) ([]byte, error) {
			if reregistered {
				signed, err := sign()
				if err != nil {
					return nil, err
				}
				requestObject = signed
			}
			return json.Marshal(createRequestBody{Request: requestObject, SessionID: sessionIDPtr, GitHubApp: narrowing})
		}
	}
	result, raw, err := c.postRequest(ctx, signer, body(narrowing))
	if narrowing == nil || !brokerTookNoNarrowing(err) {
		return result, raw, err
	}
	return c.postRequest(ctx, signer, body(nil))
}

// postRequest is one POST /v1/requests with the body build makes (doProofBuilt), answering the
// decoded result and the exact bytes the broker sent.
func (c *client) postRequest(ctx context.Context, signer Signer, build func(reregistered bool) ([]byte, error)) (RequestResult, []byte, error) {
	_, raw, err := c.doProofBuilt(ctx, signer, http.MethodPost, "/v1/requests", build)
	if err != nil {
		return RequestResult{}, nil, err
	}
	var result RequestResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return RequestResult{}, nil, fmt.Errorf("decode create request response: %w", err)
	}
	return result, raw, nil
}

type requestDecision struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

// RequestStatus is GET /v1/requests/{id}'s exact response shape. ApproveURL is as RequestResult's,
// and so is Detail: the broker's own sentence about the refusal, nil when it recorded none.
type RequestStatus struct {
	State      string           `json:"state"`
	GrantID    *string          `json:"grant_id"`
	RecordID   *string          `json:"record_id"`
	Approver   *string          `json:"approver"`
	DecidedAt  *time.Time       `json:"decided_at"`
	Decision   *requestDecision `json:"decision"`
	Detail     *string          `json:"detail"`
	ApproveURL string           `json:"approve_url"`
}

// refusedBeforeCall is a refusal this client made before it sent anything, so refusalText adds
// no "asked the broker" ending to it.
type refusedBeforeCall struct{ text string }

func (e *refusedBeforeCall) Error() string { return e.text }

// pathID refuses id, naming it as kind, when it would not appear in a call's path as written: the
// proof signs the URL this client builds, and the broker checks it against its public URL and the
// path it decoded, so an id url.PathEscape would change (a '%', a space, a '/') could only be
// escaped into a proof the broker refuses, or sent raw into a different path.
func pathID(kind, id string) error {
	if url.PathEscape(id) != id {
		return &refusedBeforeCall{fmt.Sprintf("%q is not %s", id, kind)}
	}
	return nil
}

func (c *client) GetRequest(ctx context.Context, signer Signer, id string) (RequestStatus, []byte, error) {
	if err := pathID("a request id", id); err != nil {
		return RequestStatus{}, nil, err
	}
	raw, err := c.doProof(ctx, signer, http.MethodGet, "/v1/requests/"+id, nil)
	if err != nil {
		return RequestStatus{}, nil, err
	}
	var status RequestStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return RequestStatus{}, nil, fmt.Errorf("decode request status: %w", err)
	}
	return status, raw, nil
}

func (c *client) CancelRequest(ctx context.Context, signer Signer, id string) error {
	if err := pathID("a request id", id); err != nil {
		return err
	}
	_, err := c.doProof(ctx, signer, http.MethodPost, "/v1/requests/"+id+"/cancel", nil)
	return err
}

// --- POST /v1/grants/{id}/values, POST /v1/grants/{id}/revoke ---

// GitHubNarrowing is POST /v1/grants/{id}/values's github_app object: one release's narrowing of
// its GitHub App tokens, as the request names it and as the answer names what the broker applied.
// POST /v1/requests takes the same object, where it says which narrowing the release will ask
// for, so the broker checks it before a person is asked to approve the request.
type GitHubNarrowing struct {
	Owner        string   `json:"owner"`
	Repositories []string `json:"repositories,omitempty"`
	ReadOnly     bool     `json:"read_only,omitempty"`
}

// GrantValues is POST /v1/grants/{id}/values's exact response shape. GitHubApp is the narrowing
// the broker applied: nil when it applied none, and always from a broker from before narrowing.
// Airlock is how to reach the values this release did not hand over, nil when every granted name
// was handed over — which is every release of a broker with no rules file.
type GrantValues struct {
	Values    map[string]string `json:"values"`
	ExpiresAt *time.Time        `json:"expires_at"`
	GitHubApp *GitHubNarrowing  `json:"github_app"`
	Airlock   *AirlockRelease   `json:"airlock"`
}

// AirlockRelease is POST /v1/grants/{id}/values's airlock object: how a session reaches the
// values the airlock holds back — the proxy it sends their requests through, the certificate
// authority that proxy's certificates are signed by, the password every connection to it opens
// with, and one entry per name held back. A name this object holds is released: it is in
// "names" and in no other field of the answer, never in "values".
type AirlockRelease struct {
	// The URL the command points HTTPS_PROXY at.
	ProxyURL string `json:"proxy_url"`
	// The PEM certificate the command must trust to reach the proxy, which signs a certificate
	// for each host it carries a request to.
	CACertificate string `json:"ca_certificate"`
	// The password every proxy connection authenticates with: one of this grant's own
	// placeholders, the first (name, slot) pair in sorted order.
	ProxyPassword string `json:"proxy_password"`
	// Each name the airlock holds back, by the name the command knows it as.
	Names map[string]AirlockName `json:"names"`
}

// AirlockName is one name the airlock holds back.
type AirlockName struct {
	// What the command's environment variable of that name holds: the released shape of the
	// secret with a placeholder wherever its value would be.
	Value string `json:"value"`
	// Each slot of the secret and the placeholder standing for it, so a command that takes one
	// value of the secret apart knows which placeholder is which.
	Slots map[string]string `json:"slots"`
	// The hosts the secret's airlock entry allows, which the command keeps off its NO_PROXY so
	// their requests reach the proxy.
	Hosts []string `json:"hosts"`
}

// releaseCall is what this client says of one release to the broker, which logs it with the
// grant's reason and decides nothing by it: the command the release starts, nil for none, and
// the reason this call gave, "" for none. That reason can differ from the grant's, since a call
// that asks again for names its session holds a live grant of gets that grant back. The exec form
// sends the command as record.ClipCommand leaves it, the 32 arguments of at most 256 bytes the
// broker records, so no command takes the body past the broker's 1 MiB cap and fails a release
// that would have succeeded without it.
type releaseCall struct {
	Command []string `json:"command,omitempty"`
	Reason  string   `json:"reason,omitempty"`
}

// says reports whether call names anything, which is what puts it in a release's body.
func (call releaseCall) says() bool { return len(call.Command) > 0 || call.Reason != "" }

// GrantValues releases grantID's values, asking the broker to narrow its GitHub App tokens when
// narrowing is not nil, and naming what call says of the release; with neither it sends no body,
// the release as the grant holds it. A broker from before the command or the reason field
// refuses a body holding it, 400 INVALID_NARROWING naming the unknown field, and releases nothing
// (callRefused), so the same release is sent once more without either: a newer client releases
// from an older broker as before, only unlogged. This stays: a client can meet an older broker
// on every future independent rollout.
func (c *client) GrantValues(ctx context.Context, signer Signer, grantID string, narrowing *GitHubNarrowing, call releaseCall) (GrantValues, error) {
	if err := pathID("a grant id", grantID); err != nil {
		return GrantValues{}, err
	}
	raw, err := c.release(ctx, signer, grantID, narrowing, call)
	if call.says() && callRefused(err) {
		raw, err = c.release(ctx, signer, grantID, narrowing, releaseCall{})
	}
	if err != nil {
		return GrantValues{}, err
	}
	var values GrantValues
	if err := json.Unmarshal(raw, &values); err != nil {
		return GrantValues{}, fmt.Errorf("decode grant values: %w", err)
	}
	return values, nil
}

// release is one POST /v1/grants/{id}/values, its body holding narrowing and what call says
// where each is given, and no body when none is.
func (c *client) release(ctx context.Context, signer Signer, grantID string, narrowing *GitHubNarrowing, call releaseCall) ([]byte, error) {
	var body []byte
	if narrowing != nil || call.says() {
		encoded, err := json.Marshal(struct {
			GitHubApp *GitHubNarrowing `json:"github_app,omitempty"`
			releaseCall
		}{narrowing, call})
		if err != nil {
			return nil, fmt.Errorf("encode the release: %w", err)
		}
		body = encoded
	}
	return c.doProof(ctx, signer, http.MethodPost, "/v1/grants/"+grantID+"/values", body)
}

// callRefused reports whether err is a broker from before the command or the reason field
// refusing a release for holding one: its body reader answers 400 INVALID_NARROWING, the release
// route's code for a body it cannot read, with the JSON decoder's own words for a field it does
// not declare, before anything is released.
func callRefused(err error) bool {
	var refusal *apiError
	return errors.As(err, &refusal) && refusal.Status == http.StatusBadRequest && refusal.Code == "INVALID_NARROWING" &&
		(refusal.says(`unknown field "command"`) || refusal.says(`unknown field "reason"`))
}

func (c *client) RevokeGrant(ctx context.Context, signer Signer, grantID string) error {
	if err := pathID("a grant id", grantID); err != nil {
		return err
	}
	_, err := c.doProof(ctx, signer, http.MethodPost, "/v1/grants/"+grantID+"/revoke", nil)
	return err
}

// --- GET /v1/enrollments/self, POST /v1/enrollments/{id}/renew ---

// Grant mirrors requests.Grant's wire shape.
type Grant struct {
	GrantID   string     `json:"grant_id"`
	RequestID string     `json:"request_id"`
	Approver  *string    `json:"approver"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// SelfResult is GET /v1/enrollments/self's exact response shape.
type SelfResult struct {
	EnrollmentID   string    `json:"enrollment_id"`
	Kind           string    `json:"kind"`
	Operator       *string   `json:"operator"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	Grants         []Grant   `json:"grants"`
}

func (c *client) Self(ctx context.Context, signer Signer) (SelfResult, []byte, error) {
	raw, err := c.doProof(ctx, signer, http.MethodGet, "/v1/enrollments/self", nil)
	if err != nil {
		return SelfResult{}, nil, err
	}
	var self SelfResult
	if err := json.Unmarshal(raw, &self); err != nil {
		return SelfResult{}, nil, fmt.Errorf("decode self: %w", err)
	}
	return self, raw, nil
}

// RenewEnrollment is the one Signer-taking method that still needs enrollmentID explicitly: the
// broker route is /v1/enrollments/{id}/renew, so the id is in the URL path, not just the signed
// payload. A pod's renewal carries its current projected token as pod_token; an empty podToken
// sends no body at all, which is every other session's renewal.
func (c *client) RenewEnrollment(ctx context.Context, signer Signer, enrollmentID, podToken string) (time.Time, error) {
	if err := pathID("an enrollment id", enrollmentID); err != nil {
		return time.Time{}, err
	}
	var body []byte
	if podToken != "" {
		var err error
		if body, err = json.Marshal(map[string]string{"pod_token": podToken}); err != nil {
			return time.Time{}, err
		}
	}
	_, raw, err := c.proofCall(ctx, signer, http.MethodPost, "/v1/enrollments/"+enrollmentID+"/renew", body)
	if err != nil {
		return time.Time{}, err
	}
	var result struct {
		LeaseExpiresAt time.Time `json:"lease_expires_at"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return time.Time{}, fmt.Errorf("decode renew response: %w", err)
	}
	return result.LeaseExpiresAt, nil
}

// --- GET /v1/settings, POST /v1/secrets/{name}/reread (public: no proof) ---

// Settings is GET /v1/settings's exact response shape: the store and namespace every agent secret
// lives under and, per store, the key they are encrypted with and where they are; on either store,
// the Google exchange audience a machine's own identity token must carry, from which the use
// command finds that identity with no setting (machineSource); and whether the broker reads a
// secret's type from the type tag (SecretTypeTag), without which the secret forms write no typed
// secret (requireTypeTag). A broker from before the store field answers none, and holds them in
// AWS Secrets Manager; one from before the audience, or one that accepts no machine identity,
// names none; and one from before the type tag answers no secret_type_tag.
type Settings struct {
	Store                  string `json:"store,omitempty"`
	SecretsPrefix          string `json:"secrets_prefix"`
	KMSKeyARN              string `json:"kms_key_arn,omitempty"`
	AWSAccountID           string `json:"aws_account_id,omitempty"`
	AWSRegion              string `json:"aws_region,omitempty"`
	KMSKeyName             string `json:"kms_key_name,omitempty"`
	GCPProject             string `json:"gcp_project,omitempty"`
	GoogleExchangeAudience string `json:"google_exchange_audience,omitempty"`
	SecretTypeTag          bool   `json:"secret_type_tag,omitempty"`
}

func (c *client) Settings(ctx context.Context) (Settings, error) {
	raw, err := c.do(ctx, http.MethodGet, c.route("/v1/settings"), nil, nil)
	if err != nil {
		return Settings{}, err
	}
	var settings Settings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return Settings{}, fmt.Errorf("decode settings: %w", err)
	}
	return settings, nil
}

// Reread is POST /v1/secrets/{name}/reread's exact response shape: whether the broker serves the
// secret now, as which type ("" for a stored secret, and from a broker older than the field), and,
// when it does not serve it, why.
type Reread struct {
	Name   string `json:"name"`
	Served bool   `json:"served"`
	Reason string `json:"reason"`
	Type   string `json:"type"`
}

// RereadSecret asks the broker to read the secret name from Secrets Manager now, as whoever just
// wrote it does, so the write is served (or gone) at once instead of at its next reload. name is a
// validated agent secret name, so it needs no escaping in the path.
func (c *client) RereadSecret(ctx context.Context, name string) (Reread, error) {
	raw, err := c.do(ctx, http.MethodPost, c.route("/v1/secrets/"+name+"/reread"), nil, nil)
	if err != nil {
		return Reread{}, err
	}
	var reread Reread
	if err := json.Unmarshal(raw, &reread); err != nil {
		return Reread{}, fmt.Errorf("decode reread: %w", err)
	}
	return reread, nil
}

// --- GET /v1/operator/machines, POST /v1/operator/machines/{id}/revoke,
// GET /v1/operator/grants, POST /v1/operator/grants/{id}/revoke (launcher proof) ---

// OperatorMachine mirrors one machine login in GET /v1/operator/machines, the broker's
// launcherCredentialResp.
type OperatorMachine struct {
	CredentialID string    `json:"credential_id"`
	Host         string    `json:"host"`
	Service      *string   `json:"service"`
	ApprovedBy   string    `json:"approved_by"`
	IssuedAt     time.Time `json:"issued_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Expired      bool      `json:"expired"`
}

// OperatorMachines is GET /v1/operator/machines's exact response shape.
type OperatorMachines struct {
	Credentials []OperatorMachine `json:"credentials"`
}

// OperatorMachines lists the calling machine login's operator's own machines' logins, as
// Dispatch's machine-login page lists them but without any service's login, with the raw body
// for --json.
func (c *client) OperatorMachines(ctx context.Context, signer proofSigner) (OperatorMachines, []byte, error) {
	raw, err := c.doProof(ctx, signer, http.MethodGet, "/v1/operator/machines", nil)
	if err != nil {
		return OperatorMachines{}, nil, err
	}
	var out OperatorMachines
	if err := json.Unmarshal(raw, &out); err != nil {
		return OperatorMachines{}, nil, fmt.Errorf("decode machine logins: %w", err)
	}
	return out, raw, nil
}

// RevokeOperatorMachine ends one of the calling machine login's operator's own machines' logins.
// An id that would not name one path segment as written is refused (pathID).
func (c *client) RevokeOperatorMachine(ctx context.Context, signer proofSigner, id string) error {
	if err := pathID("a machine id", id); err != nil {
		return err
	}
	_, err := c.doProof(ctx, signer, http.MethodPost, "/v1/operator/machines/"+id+"/revoke", nil)
	return err
}

// OperatorGrantEnrollment mirrors a grant's session, the broker's recordEnrollmentResp.
type OperatorGrantEnrollment struct {
	Kind      string  `json:"kind"`
	RuntimeID string  `json:"runtime_id"`
	Operator  string  `json:"operator"`
	Slot      *string `json:"slot"`
}

// OperatorGrant mirrors one grant in GET /v1/operator/grants, the broker's approverGrantResp.
type OperatorGrant struct {
	GrantID    string                  `json:"grant_id"`
	Granted    string                  `json:"granted"`
	RecordID   *string                 `json:"record_id"`
	Enrollment OperatorGrantEnrollment `json:"enrollment"`
	Names      []string                `json:"names"`
	Approver   *string                 `json:"approver"`
	// DecidedBy is who decided the request this grant rests on: the approving person's own
	// login, or "judge:<model id>" where the model judge approved it in that person's stead.
	// Null for an automatic grant, which nobody decided. It differs from Approver for a judged
	// grant alone, which records the record's own approver — the secret's owner, or anyone —
	// and that is what makes the two worth printing side by side.
	DecidedBy *string    `json:"decided_by"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
}

// OperatorGrants is GET /v1/operator/grants's exact response shape.
type OperatorGrants struct {
	Grants []OperatorGrant `json:"grants"`
}

// OperatorGrants lists the live grants of the calling machine login's operator's sessions and those
// the operator approved, as Dispatch's Live grants page lists them, with the raw body for --json.
func (c *client) OperatorGrants(ctx context.Context, signer proofSigner) (OperatorGrants, []byte, error) {
	raw, err := c.doProof(ctx, signer, http.MethodGet, "/v1/operator/grants", nil)
	if err != nil {
		return OperatorGrants{}, nil, err
	}
	var out OperatorGrants
	if err := json.Unmarshal(raw, &out); err != nil {
		return OperatorGrants{}, nil, fmt.Errorf("decode grants: %w", err)
	}
	return out, raw, nil
}

// RevokeOperatorGrant ends grant id as the calling machine login's operator. An id that would
// not name one path segment as written is refused (pathID).
func (c *client) RevokeOperatorGrant(ctx context.Context, signer proofSigner, id string) error {
	if err := pathID("a grant id", id); err != nil {
		return err
	}
	_, err := c.doProof(ctx, signer, http.MethodPost, "/v1/operator/grants/"+id+"/revoke", nil)
	return err
}
