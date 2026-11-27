// packages/envoy/cmd/cipher/client.go
//
// client is agent-secrets's own thin HTTP client for the broker routes it needs, as the broker's
// HTTP API reference (https://cybermonklabs.github.io/legion/broker/reference/api/) defines them. Session routes
// — everything but enrollment issuance/revocation and launcher-credential issuance — are signed
// per call through a Signer (proofsource.go: an agent box or pod's own key, or a machine session's
// agent-secrets-helper), producing exactly the Proof header internal/cipher/proof.Verifier
// expects. CreateRequest additionally needs a signed credential-request object (record.Sign)
// embedded in its body, since the broker carries authorization_details and
// reason inside that signed object rather than as plain top-level JSON fields; building it is
// also a Signer responsibility (Signer.SignRequestObject), since a machine session's key never
// leaves agent-secrets-helper.
// Enrollment issuance/revocation and launcher-credential issuance are helper-socket operations
// (see cmdEnrollHelper/cmdUnenrollHelper in enroll.go, cmdMachineLogin in machine.go, and
// internal/cipher/helper): no route here ever carries a bearer launcher token, and the helper's
// machine login (an OAuth device grant) carries no launcher credential (a launcher has none yet). The
// operator routes (/v1/operator/*) are signed with a launcher proof the helper's sign-launcher op
// makes (launcherSigner). Every method that a subcommand may need to print verbatim under --json
// (request, status, self, the operator lists) returns both its decoded result and the exact raw
// response bytes, so no command re-marshals a Go struct in place of the wire body.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/leasetime"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/redirects"
)

type client struct {
	baseURL cipheraddr.Address
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
func newClient(baseURL cipheraddr.Address) *client {
	return &client{baseURL: baseURL, http: redirects.Refused(&http.Client{Timeout: leasetime.RenewClientTimeout})}
}

// addressNotURL is the fixed words of every refusal of a broker address that is no absolute URL
// with a host (target, send, signRequestFor): they print no part of the address.
const addressNotURL = "the broker's address is not a valid URL"

// target is path's URL at the broker: path, with the query of this client's own it may end in,
// joined onto the address as configured (cipheraddr.Address.Join), so the address's path stays
// ahead of path and its own query ahead of path's, where path appended to the address's text would
// sit inside that query. It is sent and signed (proof.TargetURI of it), never printed: a message
// names it through c.baseURL.Call. An address that is no absolute URL with a host is refused as
// one, before anything is signed or sent, and no part of it is printed.
func (c *client) target(method, path string) (*url.URL, error) {
	target, err := c.baseURL.Join(path)
	if err != nil {
		return nil, &unreachableError{addressNotURL, c.baseURL, method, path, errNothingSent, ""}
	}
	return target, nil
}

// apiError is the broker's {"code":...,"error":...,"answer_id":...} error envelope, plus the HTTP
// status it came with. The answer id is the id the broker's own record of the answer names: the
// body's answer_id, else the X-Broker-Answer-Id header every answer the broker itself writes
// carries, so a refusal whose body is not the envelope (the broker's 404 for a path it does not
// serve) still names it. A broker from before the id sends neither, and neither does an answer
// net/http writes before any of the broker's code runs, such as its 400 for a malformed request,
// 431 and 417.
//
// code, said and answerID are the answer's code, words and answer id, held as redact.RemoteText,
// which the refusals this client tells apart compare (hasCode, says) and which prints only the
// rule's reading of them, under every verb, in JSON and in a log field. Error, Format, LogValue
// and answered print them only as the address lets them print: beside an address that is not
// clean, whoever answers can repeat the credential the address carries in any text it answers, so
// withheld is set where doStatus builds the refusal, and the refusal prints as its status and
// cipheraddr.WordsNotPrinted alone, with neither its code nor its answer id, as the token client
// prints such an answer (internal/cipherclient). The one line that names a code whatever the
// address, renew's exit on a terminal refusal, names one of terminalRenewal's, which this client
// matched it against (terminalCode).
type apiError struct {
	Status   int
	code     redact.RemoteText
	said     redact.RemoteText
	answerID redact.RemoteText
	// withheld is whether the answer came from an address that is not clean, so that none of
	// its text prints: the decision doStatus made, read by Error and answered.
	withheld bool
}

// newAPIError is a refusal with status and the answer's code, words and answer id; withheld is
// whether it came from an address that is not clean.
func newAPIError(status int, code, words, answerID redact.RemoteText, withheld bool) *apiError {
	return &apiError{Status: status, code: code, said: words, answerID: answerID, withheld: withheld}
}

// words is field as the refusal prints it: cipheraddr.WordsNotPrinted where the answer's text is
// withheld, and otherwise as the field prints itself (redact.Field).
func (e *apiError) words(field redact.RemoteText) string {
	if e.withheld {
		return cipheraddr.WordsNotPrinted
	}
	return field.String()
}

// Error is the broker's words and code, and the answer id to quote when it named one; where the
// answer's text is withheld, the status and cipheraddr.WordsNotPrinted alone. Each of the three is
// the broker's, so each prints as the rule for remote text reads it, on one line.
func (e *apiError) Error() string {
	if e.withheld {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.words(e.said))
	}
	var text string
	if e.code.IsEmpty() {
		text = fmt.Sprintf("HTTP %d: %s", e.Status, e.words(e.said))
	} else {
		text = fmt.Sprintf("%s (%s)", e.words(e.said), e.words(e.code))
	}
	if !e.answerID.IsEmpty() {
		text += " (answer " + e.words(e.answerID) + ")"
	}
	return text
}

// answered is the refusal as the lines that name its status first print it: the status, the code
// (UNKNOWN where the broker named none) and the words, or the status and
// cipheraddr.WordsNotPrinted where the answer's text is withheld.
func (e *apiError) answered() string {
	if e.withheld {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.words(e.said))
	}
	code := "UNKNOWN"
	if !e.code.IsEmpty() {
		code = e.words(e.code)
	}
	return fmt.Sprintf("HTTP %d %s: %s", e.Status, code, e.words(e.said))
}

// Format prints Error under the verb, flags, width and precision it is given, as fmt prints that
// string, so no verb prints this refusal's fields.
func (e *apiError) Format(f fmt.State, verb rune) {
	fmt.Fprintf(f, fmt.FormatString(f, verb), e.Error())
}

// LogValue is Error, so a structured log field names the refusal only as a message may.
func (e *apiError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// hasCode reports whether the broker answered code, compared as it came. It answers no text.
func (e *apiError) hasCode(code string) bool { return e.code.Equal(code) }

// says reports whether the broker's error text holds phrase. A refusal this client made itself
// (a redirect) holds no broker text, and says nothing.
func (e *apiError) says(phrase string) bool { return e.said.Contains(phrase) }

// enrollmentEnded reports whether this refusal is the broker's answer to a proof naming an
// enrollment it no longer holds: the lease lapsed, or it was revoked. Only the renew route
// answers ENROLLMENT_NOT_LIVE; every other session route says PROOF_INVALID with "not live" in
// its message, which no recoverable proof failure (iat skew, key, URL) says.
func (e *apiError) enrollmentEnded() bool {
	return e.hasCode(enrollmentEndedCode) ||
		(e.hasCode(terminalRenewal.LegacyCode) && e.says(terminalRenewal.LegacyMessage))
}

// terminalCode is the code of terminalRenewal's this refusal matched - one of its Codes, or the
// legacy code of an ended enrollment - and "" for any other refusal: a code this client wrote, so
// a line names it whatever the address.
func (e *apiError) terminalCode() string {
	for _, code := range terminalRenewal.Codes {
		if e.hasCode(code) {
			return code
		}
	}
	if e.enrollmentEnded() {
		return terminalRenewal.LegacyCode
	}
	return ""
}

// Permanent reports whether this refusal is one of terminalRenewal's: a code it lists, or the
// ended enrollment its legacy code spells (enrollmentEnded).
func (e *apiError) Permanent() bool { return e.terminalCode() != "" }

// do sends method to path at the broker (target) and, on a 2xx response, returns its answer
// (*redact.Answer); any other status is an *apiError: the broker's error envelope decoded, or, for
// a 3xx, the redirect this client does not follow.
func (c *client) do(ctx context.Context, method, path string, body []byte, headers map[string]string) (*redact.Answer, error) {
	_, answer, err := c.doStatus(ctx, method, path, body, headers)
	return answer, err
}

// doStatus is do, also answering the 2xx status itself, which tells apart the two answers a
// route gives for success — POST /v1/enrollments's 201 for a new enrollment and 200 for the one
// this key already holds.
func (c *client) doStatus(ctx context.Context, method, path string, body []byte, headers map[string]string) (int, *redact.Answer, error) {
	target, err := c.target(method, path)
	if err != nil {
		return 0, nil, err
	}
	return c.send(ctx, method, target, body, headers)
}

// send is one request of method to target, a URL target built, answered as doStatus answers it.
//
// A failure names the call by the URL its proof names (proof.TargetURI), without the query: the
// queries this client sends are the address's own, which no clean address holds, and the one it
// builds itself (GetRequest's wait), and the redactor prints no URL that holds a query, so a read
// that asked the broker to hold it would otherwise name neither its call nor its cause.
func (c *client) send(ctx context.Context, method string, target *url.URL, body []byte, headers map[string]string) (int, *redact.Answer, error) {
	named, query := proof.TargetURI(target), target.RawQuery
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		// target parsed the URL already, so what fails here is the method, or the URL as String
		// writes it, which url.Parse can refuse where it took the address (an escape in an IPv6
		// zone). With a base that prints as written, the error says which (through Error, which
		// prints it as net/url wrote it only where that is clean). With any other base, the
		// parser's own text quotes the URL, its user information included, so the message says
		// only that the address is not valid.
		if c.baseURL.Clean() {
			return 0, nil, fmt.Errorf("the request could not be built: %w", c.baseURL.Error(err))
		}
		return 0, nil, &unreachableError{addressNotURL, c.baseURL, method, named, errNothingSent, ""}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, unreachable(c.baseURL, method, named, withoutQuery(err, query))
	}
	defer resp.Body.Close()
	answer, err := redact.ReadAnswer(resp, 16<<20)
	if err != nil {
		return 0, nil, &cutOffError{base: c.baseURL, method: method, url: named, cause: c.baseURL.Error(err)}
	}
	if resp.StatusCode/100 == 3 {
		// A redirect this client refuses to follow (redirects.Refused), handed back as the
		// non-2xx it is. Its body is not printed: net/http writes the target into the body
		// of a redirect it answers a GET with (`<a href="…">Found</a>`), and that target,
		// with whatever query it carries, is what refusing the redirect keeps out of this
		// process. The status alone says what happened.
		return resp.StatusCode, nil, &apiError{Status: resp.StatusCode, said: redirectRefused}
	}
	if resp.StatusCode/100 != 2 {
		var envelope struct {
			Code     redact.RemoteText `json:"code"`
			Error    redact.RemoteText `json:"error"`
			AnswerID redact.RemoteText `json:"answer_id"`
		}
		_ = answer.Decode(&envelope)
		code, said, answerID := envelope.Code, envelope.Error, envelope.AnswerID
		if said.IsEmpty() {
			// A body that was not the envelope is the message as it came (the mux's "404 page
			// not found\n"), cut at its surrounding whitespace before it prints, which would
			// otherwise quote it for its line ending, so the id goes after it on the same line.
			said = answer.Words()
		}
		if answerID.IsEmpty() {
			answerID = redact.NewRemoteText(resp.Header.Get(answerIDHeader)).WithoutTokens(redact.Carried(resp)...)
		}
		// Beside an address that is not clean, whoever answers can repeat its credential in
		// any text of its answer: the refusal prints as its status alone (apiError.withheld).
		return resp.StatusCode, nil, newAPIError(resp.StatusCode, code, said, answerID, !c.baseURL.Clean())
	}
	return resp.StatusCode, &answer, nil
}

// redirectRefused is what a refusal of a redirect this client does not follow says: its own
// words, which the rule for remote text prints as they are.
var redirectRefused = redact.NewRemoteText("a redirect, which this client does not follow")

// answerIDHeader is the header every answer the broker itself writes names its answer id in
// (answer.Header in internal/cipher/answer, which this client does not link); net/http's own
// 400, 431 and 417, written before any of the broker's code runs, carry none.
const answerIDHeader = "X-Broker-Answer-Id"

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

// doProof signs the request with signer.Sign against exactly this method and proof.TargetURI of
// the request's URL, the shape proof.Verifier.Verify compares htm/htu against: the broker rebuilds
// the htu from BROKER_PUBLIC_URL and the request's path, so the signed URL holds no query (the
// address's own or path's), no fragment and no user information the address carries.
func (c *client) doProof(ctx context.Context, signer proofSigner, method, path string, body []byte) (*redact.Answer, error) {
	_, answer, err := c.doProofStatus(ctx, signer, method, path, body)
	return answer, err
}

// doProofStatus is doProof, also answering the 2xx status (doStatus).
//
// A signer that can register itself again (reenroller) gets exactly one retry when the broker
// says the enrollment its proof named is not live. Nothing renews a sign-in's or a machine's own
// session enrollment otherwise, so after BROKER_LEASE_SECONDS every call on that box would fail
// with the dead id still in its record, and only signing in again would cure it — making a
// second box key and a second sign-in on the person's list. One retry, because a second refusal
// is the broker saying no to the enrollment this call just made, which no further retry changes.
func (c *client) doProofStatus(ctx context.Context, signer proofSigner, method, path string, body []byte) (int, *redact.Answer, error) {
	return c.doProofBuilt(ctx, signer, method, path, func(bool) ([]byte, error) { return body, nil })
}

// doProofBuilt is doProofStatus for a body the session's own key signs part of, which build
// makes: build(false) for the first call, and build(true) again for the retry after the session
// registered again, since a machine registers again under a key of its own (machineReregister)
// and the broker takes only what the enrollment's own key signed.
func (c *client) doProofBuilt(ctx context.Context, signer proofSigner, method, path string, build func(reregistered bool) ([]byte, error)) (int, *redact.Answer, error) {
	body, err := build(false)
	if err != nil {
		return 0, nil, err
	}
	status, answer, err := c.proofCall(ctx, signer, method, path, body)
	again, ok := signer.(reenroller)
	if !ok || !enrollmentNotLive(err) {
		return status, answer, err
	}
	if reErr := again.Reenroll(ctx); reErr != nil {
		return status, answer, fmt.Errorf("%w; registering this session again: %v", err, reErr)
	}
	if body, err = build(true); err != nil {
		return 0, nil, err
	}
	return c.proofCall(ctx, signer, method, path, body)
}

// proofCall is one signed call, which is all `renew` may make: its whole contract is to report
// the enrollment the broker no longer holds (terminalRenewal, exit 76) rather than quietly
// register another.
func (c *client) proofCall(ctx context.Context, signer proofSigner, method, path string, body []byte) (int, *redact.Answer, error) {
	target, err := c.target(method, path)
	if err != nil {
		return 0, nil, err
	}
	token, err := signer.Sign(method, proof.TargetURI(target))
	if err != nil {
		return 0, nil, &signError{err: err}
	}
	return c.send(ctx, method, target, body, map[string]string{"Proof": token})
}

// signError is proofCall's failure to sign the proof, before anything was sent: the signer's own
// error, which a caller tells apart from any answer of the broker's (askReread).
type signError struct{ err error }

func (e *signError) Error() string { return "sign proof: " + e.err.Error() }
func (e *signError) Unwrap() error { return e.err }

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
// "GCLOUD_DEMO: requester-not-listed" — which every line this CLI prints about a refusal carries,
// so a session reads that asking again will not help; nil when the broker recorded none, and from
// a broker from before the field.
//
// Each member a line prints - the ids, the state, the approver, the page and the detail - is the
// broker's text, or the text of whatever answers in its place, which can echo the request it was
// sent: its Proof header, the signed request object. Each is held as redact.RemoteText, which
// Answer.Decode holds with that request's record, so it prints only as the rule reads it with
// those credentials redacted, is told apart as it came (Equal), and an id goes back to the broker
// only as RemoteText.SendBack answers it. Secrets and RecordID print nowhere, and stay as they
// came.
type RequestResult struct {
	RequestID       redact.RemoteText  `json:"request_id"`
	State           redact.RemoteText  `json:"state"`
	Secrets         []SecretDecision   `json:"secrets"`
	GrantID         *redact.RemoteText `json:"grant_id"`
	RecordID        *string            `json:"record_id"`
	Approver        *redact.RemoteText `json:"approver"`
	LifetimeSeconds *int               `json:"lifetime_seconds"`
	Coalesced       bool               `json:"coalesced,omitempty"`
	Detail          *redact.RemoteText `json:"detail"`
	ApproveURL      redact.RemoteText  `json:"approve_url"`
	// JudgeDeciding is true while the model judge holds the request: no person is asked unless
	// the judge cannot decide it, and the answer then carries ApproveURL. Such a request carries
	// no page while it is held.
	JudgeDeciding bool `json:"judge_deciding"`
}

// createRequestBody is POST /v1/requests's exact shape in the broker's HTTP API: a signed
// request object plus one optional, unsigned field, session_id (wake-only). The v8 top-level
// "secrets"/"reason"/"issue" fields are gone — they live inside the signed request object instead,
// as does a run's target (its one detail's locations).
type createRequestBody struct {
	Request   string  `json:"request"`
	SessionID *string `json:"session_id"`
}

// CreateRequest builds and signs a credential-request object carrying one
// agent_secret authorization_detail per requested name, locations on it for a run's target (nil
// for none), and lifetimeSeconds, the grant's lifetime (nil for until the session ends)
// (Signer.SignRequestObject, audience == the broker's own base URL), then calls POST /v1/requests
// with that object nested in the body and the outer call still authenticated with a session Proof
// header. It returns both the decoded result and the exact raw response's answer, for --json's
// "print the response body verbatim" requirement (writeVerbatim). When the session registers
// again on the way (doProofBuilt), the object is signed again, by the key the session now holds.
func (c *client) CreateRequest(ctx context.Context, signer Signer, names []string, reason string, lifetimeSeconds *int, sessionID string, locations []string) (RequestResult, *redact.Answer, error) {
	sign := func() (string, error) {
		requestObject, err := signer.SignRequestObject(c.baseURL, names, reason, lifetimeSeconds, locations)
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
	return c.postRequest(ctx, signer, func(reregistered bool) ([]byte, error) {
		if reregistered {
			signed, err := sign()
			if err != nil {
				return nil, err
			}
			requestObject = signed
		}
		return json.Marshal(createRequestBody{Request: requestObject, SessionID: sessionIDPtr})
	})
}

// postRequest is one POST /v1/requests with the body build makes (doProofBuilt), answering the
// decoded result and the broker's answer as it came.
func (c *client) postRequest(ctx context.Context, signer Signer, build func(reregistered bool) ([]byte, error)) (RequestResult, *redact.Answer, error) {
	_, answer, err := c.doProofBuilt(ctx, signer, http.MethodPost, "/v1/requests", build)
	if err != nil {
		return RequestResult{}, nil, err
	}
	var result RequestResult
	if err := answer.Decode(&result); err != nil {
		return RequestResult{}, nil, fmt.Errorf("decode create request response: %w", err)
	}
	return result, answer, nil
}

// requestDecision is who decided a request and when. By is the broker's text, held as
// RequestResult's members are.
type requestDecision struct {
	By redact.RemoteText `json:"by"`
	At time.Time         `json:"at"`
}

// RequestStatus is GET /v1/requests/{id}'s exact response shape. ApproveURL is as RequestResult's,
// and so is Detail: the broker's own sentence about the refusal, nil when it recorded none. Each
// member a line prints is held as RequestResult's are, with the record of the read's request;
// RecordID prints nowhere.
type RequestStatus struct {
	State         redact.RemoteText  `json:"state"`
	GrantID       *redact.RemoteText `json:"grant_id"`
	RecordID      *string            `json:"record_id"`
	Approver      *redact.RemoteText `json:"approver"`
	DecidedAt     *time.Time         `json:"decided_at"`
	Decision      *requestDecision   `json:"decision"`
	Detail        *redact.RemoteText `json:"detail"`
	ApproveURL    redact.RemoteText  `json:"approve_url"`
	JudgeDeciding bool               `json:"judge_deciding"`
}

// refusedBeforeCall is a refusal this client made before it sent anything, so refusalText adds
// no "asked the broker" ending to it.
type refusedBeforeCall struct{ text string }

func (e *refusedBeforeCall) Error() string { return e.text }

// pathID refuses id, naming kind and never id, when it would not appear in a call's path as
// written: the proof signs the URL this client builds, and the broker checks it against its public
// URL and the path it decoded, so an id url.PathEscape would change (a '%', a space, a '/') could
// only be escaped into a proof the broker refuses, or sent raw into a different path. The id is
// not printed: an argument that is no id is where a credential pasted in the wrong place lands.
func pathID(kind, id string) error {
	if url.PathEscape(id) != id {
		return &refusedBeforeCall{"not " + kind + " (the argument is not printed: it holds a character a call's path would change)"}
	}
	return nil
}

// GetRequest reads request id. waitSeconds above 0 asks the broker to hold the read while the
// request is pending, up to that many seconds (GET /v1/requests/{id}?wait=), sent as it is: the
// caller keeps it from 1 to record.MaxRequestWaitSeconds, which the broker refuses past. 0 sends
// no wait, and the broker answers at once. A broker from before the parameter ignores it and
// answers at once either way.
func (c *client) GetRequest(ctx context.Context, signer Signer, id string, waitSeconds int) (RequestStatus, *redact.Answer, error) {
	if err := pathID("a request id", id); err != nil {
		return RequestStatus{}, nil, err
	}
	path := "/v1/requests/" + id
	if waitSeconds > 0 {
		path += "?wait=" + strconv.Itoa(waitSeconds)
	}
	answer, err := c.doProof(ctx, signer, http.MethodGet, path, nil)
	if err != nil {
		return RequestStatus{}, nil, withoutTypedID(err, "/v1/requests/(the request id, not printed)")
	}
	var status RequestStatus
	if err := answer.Decode(&status); err != nil {
		return RequestStatus{}, nil, fmt.Errorf("decode request status: %w", err)
	}
	return status, answer, nil
}

func (c *client) CancelRequest(ctx context.Context, signer Signer, id string) error {
	if err := pathID("a request id", id); err != nil {
		return err
	}
	_, err := c.doProof(ctx, signer, http.MethodPost, "/v1/requests/"+id+"/cancel", nil)
	return withoutTypedID(err, "/v1/requests/(the request id, not printed)/cancel")
}

// --- POST /v1/grants/{id}/values, POST /v1/grants/{id}/revoke ---

// GrantValues is POST /v1/grants/{id}/values's exact response shape. Locations is the grant's
// target as its request signed it, the locations record.TargetLocations made, nil for a grant with
// no target and from a broker from before the target; the exec form compares it as it came and
// prints none of it. GitHubLeftOut names, for each GitHub profile and each account of its release,
// every entry of the profile's permissions the installation on that account does not hold, which
// its token leaves out: "<permission>" or "<permission>:<level>"; nil when nothing was left out.
// Each entry is held as redact.RemoteText with the release request's record (Answer.Decode).
// Airlock is how to reach the values this release did not hand over, nil when every granted name
// was handed over — which is every release of a broker with no rules file. The keys of Values, of
// Airlock.Names and of GitHubLeftOut are the broker's text too, and encoding/json decodes a key
// into no redact.RemoteText: a line names a name or a profile only where this run asked for it
// (releasedName), and an account only as LeftOut holds it.
type GrantValues struct {
	Values        map[string]string                         `json:"values"`
	ExpiresAt     *time.Time                                `json:"expires_at"`
	Locations     []string                                  `json:"locations"`
	GitHubLeftOut map[string]map[string][]redact.RemoteText `json:"github_left_out"`
	Airlock       *AirlockRelease                           `json:"airlock"`
	// LeftOut is GitHubLeftOut as reportLeftOut prints it, one entry for each profile and account,
	// in that order. GrantValues fills it from the answer; no member of the answer does.
	LeftOut []leftOut `json:"-"`
}

// leftOut is one line of what a release's GitHub profile left out: the profile (a key of the
// answer, which a line names only where this run asked for it), one account of its release, held
// as the answer said it with the release request's record (redact.Answer.Said, since the decode
// gives a key no box), and every entry of the profile's permissions the installation on that
// account does not hold.
type leftOut struct {
	Profile string
	Account redact.RemoteText
	Entries []redact.RemoteText
}

// AirlockRelease is POST /v1/grants/{id}/values's airlock object: how a session reaches the
// values the airlock holds back — the proxy it sends their requests through, the roots that
// proxy's certificates chain to, the password every connection to it opens with, and one entry
// per name held back. A name this object holds is released: it is in "names" and in no other
// field of the answer, never in "values".
type AirlockRelease struct {
	// The URL the command points HTTPS_PROXY at.
	ProxyURL string `json:"proxy_url"`
	// The PEM root certificates the command must trust to reach the proxy, one or more blocks
	// one after another, all of which the trust bundle takes. The proxy signs a certificate for
	// each host it carries a request to.
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

// GrantValues releases grantID's values, naming what call says of the release; when it says
// nothing it sends no body, the release as the grant holds it. A broker from before the command
// or the reason field refuses a body holding it, 400 INVALID_NARROWING naming the unknown field,
// and releases nothing (callRefused), so the same release is sent once more without either: a
// newer client releases from an older broker as before, only unlogged. This stays: a client can
// meet an older broker on every future independent rollout.
func (c *client) GrantValues(ctx context.Context, signer Signer, grantID string, call releaseCall) (GrantValues, error) {
	if err := pathID("a grant id", grantID); err != nil {
		return GrantValues{}, err
	}
	answer, err := c.release(ctx, signer, grantID, call)
	if call.says() && callRefused(err) {
		answer, err = c.release(ctx, signer, grantID, releaseCall{})
	}
	if err != nil {
		return GrantValues{}, err
	}
	var values GrantValues
	if err := answer.Decode(&values); err != nil {
		return GrantValues{}, fmt.Errorf("decode grant values: %w", err)
	}
	for _, profile := range slices.Sorted(maps.Keys(values.GitHubLeftOut)) {
		accounts := values.GitHubLeftOut[profile]
		for _, account := range slices.Sorted(maps.Keys(accounts)) {
			values.LeftOut = append(values.LeftOut, leftOut{Profile: profile, Account: answer.Said(account), Entries: accounts[account]})
		}
	}
	return values, nil
}

// release is one POST /v1/grants/{id}/values, its body holding what call says, and no body when
// it says nothing.
func (c *client) release(ctx context.Context, signer Signer, grantID string, call releaseCall) (*redact.Answer, error) {
	var body []byte
	if call.says() {
		encoded, err := json.Marshal(call)
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
	return errors.As(err, &refusal) && refusal.Status == http.StatusBadRequest && refusal.hasCode("INVALID_NARROWING") &&
		(refusal.says(`unknown field "command"`) || refusal.says(`unknown field "reason"`))
}

func (c *client) RevokeGrant(ctx context.Context, signer Signer, grantID string) error {
	if err := pathID("a grant id", grantID); err != nil {
		return err
	}
	_, err := c.doProof(ctx, signer, http.MethodPost, "/v1/grants/"+grantID+"/revoke", nil)
	return withoutTypedID(err, "/v1/grants/(the grant id, not printed)/revoke")
}

// --- GET /v1/enrollments/self, POST /v1/enrollments/{id}/renew ---

// Grant mirrors requests.Grant's wire shape. Its ids print, and are held as RequestResult's
// members are; Approver prints nowhere.
type Grant struct {
	GrantID   redact.RemoteText `json:"grant_id"`
	RequestID redact.RemoteText `json:"request_id"`
	Approver  *string           `json:"approver"`
	ExpiresAt *time.Time        `json:"expires_at"`
}

// SelfResult is GET /v1/enrollments/self's exact response shape. Each member self prints is held
// as RequestResult's members are, with the record of the read's request.
type SelfResult struct {
	EnrollmentID   redact.RemoteText  `json:"enrollment_id"`
	Kind           redact.RemoteText  `json:"kind"`
	Operator       *redact.RemoteText `json:"operator"`
	LeaseExpiresAt time.Time          `json:"lease_expires_at"`
	Grants         []Grant            `json:"grants"`
	// Projects are the Dispatch projects the session named when it enrolled; none from a broker
	// from before the field.
	Projects []redact.RemoteText `json:"projects"`
}

func (c *client) Self(ctx context.Context, signer Signer) (SelfResult, *redact.Answer, error) {
	answer, err := c.doProof(ctx, signer, http.MethodGet, "/v1/enrollments/self", nil)
	if err != nil {
		return SelfResult{}, nil, err
	}
	var self SelfResult
	if err := answer.Decode(&self); err != nil {
		return SelfResult{}, nil, fmt.Errorf("decode self: %w", err)
	}
	return self, answer, nil
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
	_, answer, err := c.proofCall(ctx, signer, http.MethodPost, "/v1/enrollments/"+enrollmentID+"/renew", body)
	if err != nil {
		return time.Time{}, err
	}
	var result struct {
		LeaseExpiresAt time.Time `json:"lease_expires_at"`
	}
	if err := answer.Decode(&result); err != nil {
		return time.Time{}, fmt.Errorf("decode renew response: %w", err)
	}
	return result.LeaseExpiresAt, nil
}

// --- GET /v1/operator/machines, POST /v1/operator/machines/{id}/revoke,
// GET /v1/operator/grants, POST /v1/operator/grants/{id}/revoke (launcher proof) ---

// OperatorMachine mirrors one machine login in GET /v1/operator/machines, the broker's
// launcherCredentialResp. The members machineRow prints are held as RequestResult's members are,
// with the record of the list's launcher-proof request; Service prints nowhere.
type OperatorMachine struct {
	CredentialID redact.RemoteText `json:"credential_id"`
	Host         redact.RemoteText `json:"host"`
	Service      *string           `json:"service"`
	ApprovedBy   redact.RemoteText `json:"approved_by"`
	IssuedAt     time.Time         `json:"issued_at"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Expired      bool              `json:"expired"`
}

// OperatorMachines is GET /v1/operator/machines's exact response shape.
type OperatorMachines struct {
	Credentials []OperatorMachine `json:"credentials"`
}

// OperatorMachines lists the calling machine login's operator's own logins, their machines' and
// those of the pods whose account BROKER_POD_OPERATORS maps to them, as Dispatch's machine-login
// page lists them but without any service's login, with the raw body for --json.
func (c *client) OperatorMachines(ctx context.Context, signer proofSigner) (OperatorMachines, *redact.Answer, error) {
	answer, err := c.doProof(ctx, signer, http.MethodGet, "/v1/operator/machines", nil)
	if err != nil {
		return OperatorMachines{}, nil, err
	}
	var out OperatorMachines
	if err := answer.Decode(&out); err != nil {
		return OperatorMachines{}, nil, fmt.Errorf("decode machine logins: %w", err)
	}
	return out, answer, nil
}

// RevokeOperatorMachine ends one of the calling machine login's operator's own logins, a
// machine's or a pod's, as OperatorMachines lists them.
// An id that would not name one path segment as written is refused (pathID).
func (c *client) RevokeOperatorMachine(ctx context.Context, signer proofSigner, id string) error {
	if err := pathID("a machine id", id); err != nil {
		return err
	}
	_, err := c.doProof(ctx, signer, http.MethodPost, "/v1/operator/machines/"+id+"/revoke", nil)
	return withoutTypedID(err, "/v1/operator/machines/(the machine id, not printed)/revoke")
}

// OperatorGrantEnrollment mirrors a grant's session, the broker's recordEnrollmentResp, each
// member held as OperatorGrant's are.
type OperatorGrantEnrollment struct {
	Kind      redact.RemoteText  `json:"kind"`
	RuntimeID redact.RemoteText  `json:"runtime_id"`
	Operator  redact.RemoteText  `json:"operator"`
	Slot      *redact.RemoteText `json:"slot"`
}

// OperatorGrant mirrors one grant in GET /v1/operator/grants, the broker's approverGrantResp. The
// members grantRow prints are held as RequestResult's members are, with the record of the list's
// launcher-proof request; RecordID prints nowhere.
type OperatorGrant struct {
	GrantID    redact.RemoteText       `json:"grant_id"`
	Granted    redact.RemoteText       `json:"granted"`
	RecordID   *string                 `json:"record_id"`
	Enrollment OperatorGrantEnrollment `json:"enrollment"`
	Names      []redact.RemoteText     `json:"names"`
	Approver   *redact.RemoteText      `json:"approver"`
	// DecidedBy is who decided the request this grant rests on: the approving person's own
	// login, or "judge:<model id>" where the model judge approved it in that person's stead.
	// Null for an automatic grant, which nobody decided. It differs from Approver for a judged
	// grant alone, which records the record's own approver — the secret's owner, or anyone —
	// and that is what makes the two worth printing side by side.
	DecidedBy *redact.RemoteText `json:"decided_by"`
	ExpiresAt *time.Time         `json:"expires_at"`
	CreatedAt time.Time          `json:"created_at"`
}

// OperatorGrants is GET /v1/operator/grants's exact response shape.
type OperatorGrants struct {
	Grants []OperatorGrant `json:"grants"`
}

// OperatorGrants lists the live grants of the calling machine login's operator's sessions and those
// the operator approved, as Dispatch's Live grants page lists them, with the raw body for --json.
func (c *client) OperatorGrants(ctx context.Context, signer proofSigner) (OperatorGrants, *redact.Answer, error) {
	answer, err := c.doProof(ctx, signer, http.MethodGet, "/v1/operator/grants", nil)
	if err != nil {
		return OperatorGrants{}, nil, err
	}
	var out OperatorGrants
	if err := answer.Decode(&out); err != nil {
		return OperatorGrants{}, nil, fmt.Errorf("decode grants: %w", err)
	}
	return out, answer, nil
}

// RevokeOperatorGrant ends grant id as the calling machine login's operator. An id that would
// not name one path segment as written is refused (pathID).
func (c *client) RevokeOperatorGrant(ctx context.Context, signer proofSigner, id string) error {
	if err := pathID("a grant id", id); err != nil {
		return err
	}
	_, err := c.doProof(ctx, signer, http.MethodPost, "/v1/operator/grants/"+id+"/revoke", nil)
	return withoutTypedID(err, "/v1/operator/grants/(the grant id, not printed)/revoke")
}
