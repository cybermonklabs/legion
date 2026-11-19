// packages/envoy/cmd/agent-secrets/proofsource.go
//
// Signer produces the compact JWS this process puts in a broker call's Proof header, and the
// signed credential-request object POST /v1/requests embeds (the broker puts
// authorization_details and reason inside
// that signed object, not plain top-level fields). An agent box or pod has its own key on tmpfs
// under AGENT_SECRETS_KEY_DIR and signs both directly for its own enrollment id (fileSigner), as
// do a person's sign-in and a machine's own session; a machine session has no key of its own and
// asks agent-secrets-helper, over AGENT_SECRETS_HELPER_SOCK, which signs only for processes that
// descend from a registered session root (helperSigner) — the session's key never leaves the
// helper process, so building the request object is also the helper's job in that mode.
// signerChain (chain.go) is the one place that chooses between them, and it builds a helperSigner
// only after probeHelper said the caller is in a session. The operator's own machine and
// grant commands sign as this machine's login instead, through the helper's sign-launcher op
// (launcherSigner, built by machineContext).
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/brokerclient"
)

// proofSigner signs one broker call and returns the compact JWS for its Proof header: all a call
// that asks for no secret needs, such as the machine and grant commands' (launcherSigner).
type proofSigner interface {
	Sign(method, url string) (string, error)
}

// Signer is a session's proofSigner, which also builds and signs a credential-request object
// naming the secrets the session is asking for, and the lifetime its grant lasts: lifetimeSeconds,
// or nil when it lasts until the session ends. audience is the broker the object is for.
type Signer interface {
	proofSigner
	SignRequestObject(audience brokeraddr.Address, names []string, reason string, lifetimeSeconds *int) (string, error)
}

// fileSigner signs with the key and enrollment id an agent box or pod keeps under
// AGENT_SECRETS_KEY_DIR.
//
// register is how this session is made again when the broker says the enrollment stale is not
// live, and it answers the key and the enrollment id to sign with from then on. It is set for the
// two sessions this command registers itself — a person's sign-in on this box, which registers
// its box key again, and a machine that proves an identity of its own, which registers a key of
// its own each time (machineReregister) — and nil for every other, a box or pod its launcher
// enrolled above all: that enrollment is the launcher's to make again, and registering one here
// would leave the launcher's own key dir naming a session the launcher never made.
type fileSigner struct {
	key          *ecdsa.PrivateKey
	enrollmentID string
	register     func(ctx context.Context, stale string) (*ecdsa.PrivateKey, string, error)
}

func (f *fileSigner) Sign(method, url string) (string, error) {
	return proof.Sign(f.key, f.enrollmentID, method, url, time.Now())
}

// Reenroll registers this session again and signs with the key and enrollment it gets from here
// on (client.reenroller). A signer with no register answers an error, so the broker's own refusal
// is what the caller reports.
func (f *fileSigner) Reenroll(ctx context.Context) error {
	if f.register == nil {
		return errors.New("nothing here can register this session again: its enrollment is its launcher's to make")
	}
	key, id, err := f.register(ctx, f.enrollmentID)
	if err != nil {
		return err
	}
	f.key, f.enrollmentID = key, id
	return nil
}

func (f *fileSigner) SignRequestObject(audience brokeraddr.Address, names []string, reason string, lifetimeSeconds *int) (string, error) {
	details := make([]record.AuthorizationDetail, len(names))
	for i, name := range names {
		details[i] = record.AuthorizationDetail{Type: record.KindAgentSecret, Identifier: name, Actions: []string{"inject"}}
	}
	return record.Sign(f.key, audience.Raw(), details, reason, "", lifetimeSeconds, time.Now())
}

// helperSigner asks agent-secrets-helper, over its unix socket, to sign for this process's
// registered machine session. The helper resolves the session (and its enrollment id) from the
// caller's own pid — nothing here names an enrollment id at all. audience is ignored: the
// helper signs a request object against its own configured broker URL, not a value this
// (untrusted, merely locally-scoped) peer supplies — the same trust boundary the Sign method
// already has, where the helper's own descendancy check is the only gate, never the values a
// peer names in the request itself.
type helperSigner struct {
	sock string
}

var errNoCredential = errors.New("this machine's agent-secrets-helper holds no credential at the secrets broker")

type noCredentialError struct{ helper string }

func (e *noCredentialError) Error() string {
	return errNoCredential.Error() + " (the helper: " + e.helper + ")"
}

func (e *noCredentialError) Is(target error) bool { return target == errNoCredential }

func (e *noCredentialError) sessionLine() string {
	return errNoCredential.Error() + "; not an agent session (the helper: " + e.helper + ")"
}

func noCredential(resp helper.Response) *noCredentialError {
	return &noCredentialError{helper: resp.Error}
}

// helperAnswer is what the helper said to the identity probe: sock is the socket asked, there
// whether its file exists, err a socket that is there and did not answer within
// identityDeadline, and resp the answer otherwise.
type helperAnswer struct {
	sock  string
	there bool
	resp  helper.Response
	err   error
}

// answered reports whether the helper answered the probe at all, whatever it said.
func (a helperAnswer) answered() bool { return a.there && a.err == nil }

// probeHelper asks the helper's sign op for a throwaway proof (identityProbeURL), which says
// whether the caller is inside a session the helper registered and changes nothing: OK
// (enrolled), NOT_ENROLLED (registered and still enrolling), NOT_A_SESSION, or NO_CREDENTIAL. The
// socket is AGENT_SECRETS_HELPER_SOCK, else the helper's default, and a socket file that is not
// there is asked nothing.
func probeHelper() helperAnswer {
	sock, _ := helperSocket()
	if !exists(sock) {
		return helperAnswer{sock: sock}
	}
	resp, err := helper.Call(sock, helper.Request{Op: "sign", Method: http.MethodGet, URL: identityProbeURL}, identityDeadline)
	return helperAnswer{sock: sock, there: true, resp: resp, err: err}
}

// helperRefusal is the error for a helper answer that is not OK.
func helperRefusal(resp helper.Response) error {
	if resp.Code == helper.CodeNoCredential {
		return noCredential(resp)
	}
	return fmt.Errorf("%s: %s", resp.Code, resp.Error)
}

// helperError is the local helper's own refusal or a socket that did not answer. It names the
// helper socket because no broker has been asked yet: adding a broker source trailer would report
// a request path the client did not take.
type helperError struct {
	socket  string
	err     error
	refused bool
}

func (e *helperError) Error() string {
	if e.refused {
		return fmt.Sprintf("agent-secrets-helper at %s refused: %v", e.socket, e.err)
	}
	return fmt.Sprintf("agent-secrets-helper at %s did not answer: %v", e.socket, e.err)
}

func (e *helperError) Unwrap() error { return e.err }

func helperFailure(socket string, err error, refused bool) error {
	if errors.Is(err, errNoCredential) {
		return err
	}
	return &helperError{socket: socket, err: err, refused: refused}
}

func (h *helperSigner) Sign(method, url string) (string, error) {
	resp, err := helper.Call(h.sock, helper.Request{Op: "sign", Method: method, URL: url}, 5*time.Second)
	if err != nil {
		return "", helperFailure(h.sock, err, false)
	}
	if !resp.OK {
		return "", helperFailure(h.sock, helperRefusal(resp), true)
	}
	return resp.Proof, nil
}

// SignRequestObject asks the helper's sign-request op. A helper from before lifetime_seconds
// ignores the field and signs no lifetime; the broker's answer then names none, and the caller
// that asked for one refuses it (grantedLifetime).
func (h *helperSigner) SignRequestObject(audience brokeraddr.Address, names []string, reason string, lifetimeSeconds *int) (string, error) {
	resp, err := helper.Call(h.sock, helper.Request{Op: "sign-request", Secrets: names, Reason: reason, LifetimeSeconds: lifetimeSeconds}, 5*time.Second)
	if err != nil {
		return "", helperFailure(h.sock, err, false)
	}
	if !resp.OK {
		return "", helperFailure(h.sock, helperRefusal(resp), true)
	}
	return resp.RequestObject, nil
}

// launcherRefusal is why the helper gave a machine or grant command no launcher proof, which the
// command prints as it stands: no machine login to act as (NO_CREDENTIAL), a helper from before
// sign-launcher (which answers it as an unknown op), or the helper's own code and message, such as
// IN_SESSION from inside a session.
type launcherRefusal struct{ message string }

func (e *launcherRefusal) Error() string { return e.message }

// launcherSigner signs broker calls with the machine credential through the helper's
// sign-launcher op: the key never leaves the helper, which signs only the four operator routes of
// its own broker, for a process it can show is outside every registered session's process tree.
// credentialID is the credential the last proof named, as the helper answered it, and failure the
// class of that helper's mode (helper.Response.NoCredentialFailure), which names what cures the
// broker refusing the credential.
type launcherSigner struct {
	sock         string
	credentialID string
	failure      brokerclient.Failure
}

func (l *launcherSigner) Sign(method, url string) (string, error) {
	resp, err := helper.Call(l.sock, helper.Request{Op: "sign-launcher", Method: method, URL: url}, 5*time.Second)
	if err != nil {
		return "", fmt.Errorf("agent-secrets-helper at %s: %w", l.sock, err)
	}
	switch {
	case resp.OK:
		l.credentialID, l.failure = resp.CredentialID, resp.NoCredentialFailure()
		return resp.Proof, nil
	case resp.Code == helper.CodeNoCredential:
		return "", &launcherRefusal{noCredential(resp).Error()}
	case resp.Code == helper.CodeBadRequest && resp.Error == "unknown op sign-launcher":
		return "", &launcherRefusal{"this machine's agent-secrets-helper is older than this client and cannot sign for machine and grant commands; restart it on this release"}
	}
	return "", &launcherRefusal{resp.Code + ": " + resp.Error}
}

// machineContext answers the broker URL and a launcher signer. Machine and grant commands run
// where the helper does (the devbox); a machine with no helper has no machine login to act as.
func machineContext() (base brokeraddr.Address, signer *launcherSigner, err error) {
	base = brokerBaseURL()
	if base.Empty() {
		return brokeraddr.Address{}, nil, fmt.Errorf("%s is required: it names the secrets broker this machine's login, and the grants it lists or ends, are at", brokerURLVar)
	}
	sock, named := helperSocket()
	if !exists(sock) {
		where := "the default socket; AGENT_SECRETS_HELPER_SOCK is unset"
		if named {
			where = "AGENT_SECRETS_HELPER_SOCK"
		}
		return brokeraddr.Address{}, nil, fmt.Errorf("no agent-secrets-helper at %s (%s): machine and grant commands act under this machine's login, which its helper holds", sock, where)
	}
	return base, &launcherSigner{sock: sock}, nil
}
