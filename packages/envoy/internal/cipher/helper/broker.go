// packages/envoy/internal/cipher/helper/broker.go
//go:build linux

package helper

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/ctxlock"
	"github.com/sjawhar/envoy/internal/oauthrefusal"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/serverwait"
	"github.com/sjawhar/envoy/internal/urljoin"
	"github.com/sjawhar/envoy/redirects"
)

// operatorFileNamed is how every message names the operator file: by the setting that names it,
// never by its path, as MachineFileNamed names the machine file.
const operatorFileNamed = "the operator file AGENT_SECRETS_OPERATOR_FILE names (default ~/.config/agent-secrets/operator)"

var errNoCredential = errors.New("no machine credential")

func (b *Broker) noCredentialFailure() cipherclient.Failure {
	return modeFailure(b.Identity != nil, b.identityStopped.Load())
}

func (b *Broker) noCredential() error {
	return cipherclient.WithHint(errNoCredential, b.noCredentialFailure())
}

func (b *Broker) noCredentialText() string {
	return b.noCredential().Error()
}

// The refresh loop's defaults: a refresh a day at most; after a refresh the broker did not answer
// (any transport error: a refused, unanswered or unresolved dial, a timeout, a lost answer, a
// cancelled request) or answered 5xx, a retry every 15 s until an answer settles it (a success or
// invalid_grant), answered refusals between included, so a lost answer's rotated token is redeemed
// again inside the broker's five-minute rotation grace whenever the outage is shorter than that
// grace; after a 429, its Retry-After, at least 15 s and at most a minute; after any other error
// the broker answered, a minute, doubling to an hour.
const (
	defaultRefreshEvery = 24 * time.Hour
	defaultRefreshRetry = 15 * time.Second
	// maxRetryAfter caps the wait a 429's Retry-After asks for, so no hop's large value stops the
	// refresh or pushes a retry past the broker's rotation grace.
	maxRetryAfter = time.Minute
	// defaultRefreshBackoff is the first wait after a refresh the broker refused with an error
	// other than a 429 or invalid_grant, doubling to maxRefreshRetry.
	defaultRefreshBackoff = time.Minute
	maxRefreshRetry       = time.Hour
	// defaultStartRefreshBound bounds Start's refresh of the kept login, so serve listens well
	// inside the 10 s `agent-secrets register` waits for the socket.
	defaultStartRefreshBound = 5 * time.Second
)

// machineCredential is the helper's launcher identity: the machine key, the launcher credential
// the broker minted once a human approved the machine login, when that credential lapses unless a
// refresh moves it, and the refresh token the next refresh spends. The same login is kept on disk
// (MachineFile) so a restart resumes it. A refresh replaces the held machineCredential with one
// carrying the new expiry and refresh token; gone closes when the helper stops holding it.
type machineCredential struct {
	key          *ecdsa.PrivateKey
	id           string
	expiresAt    time.Time
	refreshToken string
	gone         chan struct{}
}

// credential is the in-memory credential f describes.
func (f *MachineFile) credential() *machineCredential {
	return &machineCredential{key: f.Key, id: f.CredentialID, expiresAt: f.ExpiresAt.UTC(), refreshToken: f.RefreshToken, gone: make(chan struct{})}
}

// loginState is the machine login as login-status reports it: the most recent login's
// human-facing confirmation code and its state, beside whether the helper holds a launcher
// credential and when that credential expires. Those are separate facts, since a re-login that is
// denied, expires unapproved or is still pending leaves the credential an earlier login installed
// in place, and the helper keeps enrolling sessions with it. A login the helper resumed from its
// machine file at start reads "issued" with no code. Dropped is why the helper dropped the
// credential it held (dropRefused, dropRefreshRefused or dropExpired), set while no login has been
// recorded since, so the helper holds none; Refused is Dropped != "", the wire's login_refused.
// State then reads "expired" whatever the most recent login's was, unless that login is still
// pending. "expired" is the word the dotfiles launcher gate matches and the one an older client
// prints its refusal line for, and an older client must never read "issued" for a credential the
// helper no longer holds. A login pending at the drop that then settles is recorded after it, so it
// reports its own outcome, which `machine login` prints. "expired" is otherwise a pending login
// nobody approved in time.
type loginState struct {
	Code, State         string    // the most recent login; State: pending|issued|denied|expired
	VerificationURI     string    // where a person types Code: the device authorization's verification_uri; set while the login is pending
	Reason              string    // beside an "expired" State, how many polls in a row before the code lapsed the broker did not answer or answered with an outage (pollsUnreached); "" otherwise
	CredentialHeld      bool      // the helper holds a launcher credential, whichever login installed it
	CredentialExpiresAt time.Time // when the held credential lapses without a refresh; zero while none is held
	Refused             bool      // Dropped != ""
	Dropped             string    // why the helper dropped the credential it held, while no login has been recorded since
}

// Broker is the helper's view of the secrets broker: the launcher routes (Enroll, Revoke)
// authenticate with a launcher credential, and Renew authenticates with the session's own
// proof, exactly as before.
//
// Two credentials can be that launcher credential, and enrollAs chooses between them at every
// enrolment (enrolAuthority). The first is this machine's own. Where this machine proves an
// identity of its own (Identity: a Google identity token, a projected Kubernetes token, a token
// minted through a workload identity pool), that credential is the one the broker's RFC 8693
// exchange mints for it, traded at Start and traded again before it lapses, with no person in
// it at all. Where it proves none, it is the one a human-approved machine login mints (Login,
// an OAuth device grant on the broker's own token endpoint), kept at MachineFile and refreshed
// daily (Start), carrying the person who approved it.
//
// The second is the credential of the person signed in on this box (SignIns), which outranks
// the machine's: the helper runs as that person's own user, so a sign-in it can read is a
// sign-in of its own user, and the rule a session acts under is the spec's — a session acts for
// a person only after that person's own sign-in on the box.
type Broker struct {
	// URL is the broker's base URL (AGENT_SECRETS_URL), held as a cipheraddr.Address so that no
	// log line or error of the helper prints a credential it carries. Its Raw builds each
	// request and the audience and htu each proof names; nothing else reads it.
	URL          cipheraddr.Address
	OperatorFile string // login_hint source; read per login, as before
	// MachineFile is where the helper keeps its machine login across restarts (AGENT_SECRETS_MACHINE_FILE).
	MachineFile string
	// Identity, when set, is this machine's own identity at the broker's token exchange, which
	// answers a launcher credential of its own. It replaces the machine login entirely: Start
	// reads no machine file, no device grant runs, and no person approves anything. The key it
	// trades with is kept (LoadOrMakeIdentityKey), so a restart trades the same subject and the
	// broker moves the one credential this machine holds rather than minting a second beside
	// it; the renewal is a fresh identity token traded again, this grant issuing no refresh
	// token. Nil is a machine that proves nothing of its own and logs in as before.
	Identity *cipherclient.Client
	// SignIns, when set, is the people signed in on this box as this helper's own user
	// (`agent-secrets login`), asked before every enrolment: a session registered while one of
	// them is signed in enrols under that person's launcher credential and acts for them, and
	// one registered with none signed in enrols under this machine's own. It reads the store of
	// this helper's own key directory and no other user's, and nothing of it reaches a session:
	// a session never says whose authority it enrols under. Nil where no sign-in can apply —
	// this machine proves no identity of its own, so there is no actor token for a person's
	// trade, or the deployment names no sign-in client to renew a person's identity token with.
	SignIns SignIns
	HTTP    *http.Client
	// Log records every change of the launcher credential: a machine login or a trade installing
	// one, a restart resuming one, a refresh moving its expiry, the warning a day before it
	// lapses, and the helper dropping it, when the broker refuses it or its refresh or it
	// reaches its expiry. Its lines carry the credential id, its expiry, the operator the login
	// was signed with (its login_hint) and the broker's refusal code, which are identifiers,
	// never a proof, a token, a request object or key material. Nil logs nothing.
	Log *slog.Logger

	cred            atomic.Pointer[machineCredential]
	identityStopped atomic.Bool
	loginMu         sync.Mutex                 // one login at a time
	login           atomic.Pointer[loginState] // most recent login: code, state
	// credMu is held by whatever changes the held login and its machine file together: a login's
	// save and install, a start's install, and a refresh from its request to its save and swap, so
	// a refresh never races a login's install. Every save of the machine file runs under it. Every
	// holder waits for it as long as it must, except a drop (dropOutsideRefresh), which gives up
	// when its caller's context ends (LockContext): clearOnInvalid's caller is a call a client
	// waits on, and a refresh holds credMu for the whole of its token request, on the refresh
	// loop's context, which has no deadline.
	credMu ctxlock.Mutex
	// closed, set by Close under credMu, makes every later save of the machine file write nothing.
	closed bool
	// stateMu is held by every write of cred, login and dropped, and by LoginStatus while it reads
	// the three, so login-status never pairs one moment's login with another's credential, and a
	// drop is recorded with the clear that caused it. cred and login stay atomics so a reader of
	// only one (HasCredential, launcherProof, Login's pending check) takes no lock.
	stateMu sync.Mutex
	dropped string // why drop cleared cred (dropRefused, dropRefreshRefused, dropExpired); "" once a login is recorded since; guarded by stateMu
	// testClearHook, when set, runs in drop between clearing the credential and recording why.
	// testBeforeDrop, when set, runs in dropOutsideRefresh before it waits for credMu. They exist
	// only so a test can read login-status there, or know a drop is waiting on a refresh; no
	// production caller sets them.
	testClearHook, testBeforeDrop func()
	// testAfter, when set, gives watchExpiry a test's channel in place of time.After for a
	// credential it returns one for (nil keeps time.After), so a test fires an expiry's timers
	// without waiting for them. No production caller sets it.
	testAfter func(cred *machineCredential, d time.Duration) <-chan time.Time
	// testWatchEnded, when set, runs as watchExpiry returns for cred, so a test knows a watch said
	// all it will. No production caller sets it.
	testWatchEnded func(cred *machineCredential)
	// expiryWarning is how long before the credential's expiry the helper warns that no refresh
	// has moved it; zero means a day. Only tests shorten it.
	expiryWarning time.Duration
	// refreshEvery is the longest the refresh loop waits after a refresh that reached the broker
	// or a login's install (nextRefresh), and refreshRetry its wait after one the broker did not
	// answer or answered 5xx (retrySchedule); zero means a day and 15 s. Only tests shorten them.
	refreshEvery, refreshRetry time.Duration
	// startRefreshBound is how long Start waits on its refresh of the kept login; zero means
	// defaultStartRefreshBound. Only tests shorten it.
	startRefreshBound time.Duration
	// testSave, when set, replaces MachineFile.Save for every save of the machine file, so a test
	// fails a save the way a full disk does (the unlink still works). No production caller sets it.
	testSave func(f *MachineFile, path string) error
	// testLoginWait, when set, takes the place of each wait pollLogin makes before a poll: it is
	// handed the wait and the device authorization, which it may lapse, and the poll follows at once,
	// so a test reads every wait without waiting it out. No production caller sets it.
	testLoginWait func(wait time.Duration, da *oauth2.DeviceAuthResponse)
	// testLoginNow, when set, takes the place of the clock readings Login makes around the device
	// authorization's request: the moment it asks, and the two after the answer arrives. A test
	// hands it the moments, so the round trip Login measures is the one the test names, not the
	// time the runner took; the code's end is still x/oauth2's own reading of the real clock. No
	// production caller sets it.
	testLoginNow func() time.Time

	// unsaved is the held login whose save failed, while no save has landed since. The failed
	// save removed the machine file at once (unsaveFile), since it named a token the broker had
	// just rotated away and the helper cannot know how long ago the token was first rotated; until
	// a save lands, the helper retries only the save (retrySave) and makes no token request on any
	// path. Guarded by credMu.
	unsaved *machineCredential
	// refusedCred is the credential drop cleared for a 401 LAUNCHER_INVALID (dropRefused), while no
	// login has installed one since, so the next login retires it (retireReplaced) as it retires a
	// credential it replaces. Guarded by stateMu.
	refusedCred *machineCredential

	// signalsOnce makes signals, the refresh loop's wake-ups (refreshSignals).
	signalsOnce sync.Once
	signals     refreshSignals

	installedMu sync.Mutex
	installed   chan struct{} // closed, then replaced, each time a credential is installed
}

// SignIns is the people signed in on this box, as the Broker asks about them:
// cipherclient.SignInAuthority in the helper itself, and a stand-in in a test, since this
// package's own rig needs a unix socket whose peer the kernel can pin and the rule these two
// calls carry must be testable without one.
type SignIns interface {
	// Current is the launcher credential a session enrolling now acts under, nil where this
	// box offers none and the machine's own is what it enrols under. A trade that renews it
	// runs on its caller's context, which for enrollLoop has no deadline, so a call that finds
	// one in progress answers an error wrapping ctx's once ctx ends rather than waiting the
	// trade out: a broker-call op behind it (Server.brokerCall) ends at its bound with that
	// error, a known failure, since the op has made nothing (enrollAs's retry calls it only
	// after the broker refused the first request).
	Current(ctx context.Context) (*cipherclient.Authority, error)
	// Refused tells it the broker rejected a launcher proof signed with that credential, so
	// the next Current trades afresh rather than presenting it again. It answers ctx's error,
	// having recorded nothing, when ctx ends while it waits for a Current in progress.
	Refused(ctx context.Context, credentialID string) error
}

// BrokerError is a non-success answer, with the contract's code.
//
// code and said are the answer's code and words, held as redact.RemoteText, which the helper acts
// on and reads (HasCode, says) and which prints only the rule's reading of them, under every verb,
// in JSON and in a log field. Beside an address that is not clean they print not at all: whoever
// answers there can repeat the credential the address carries in any text it answers, so do and
// oauthBrokerError set withheld, and the refusal prints as its status and
// cipheraddr.WordsNotPrinted, without its code. A log line that names a code names one the helper
// matched against its own (isLauncherInvalid, enrollmentNotLive, revokeOnce).
type BrokerError struct {
	Status int
	code   redact.RemoteText
	said   redact.RemoteText
	// withheld is whether the answer came from an address that is not clean, so that none of
	// its text prints: the decision do or oauthBrokerError made, read by Error.
	withheld bool
}

// newBrokerError is a refusal with status and the broker's code and words; withheld is whether it
// came from an address that is not clean.
func newBrokerError(status int, code, words redact.RemoteText, withheld bool) *BrokerError {
	return &BrokerError{Status: status, code: code, said: words, withheld: withheld}
}

// Error names only the fields it has: the status always, then the code where there is one, then
// ": " and the words where there are any. A front door's 502 with no JSON body reads "broker 502",
// not "broker 502 : ", and an unknown outcome's answer shows that text to an operator
// (mayHaveReachedCause, server.go). With both fields set it reads "broker 403 SESSION_HELD: the
// session … belongs to …", which nocredential_test.go matches as a prefix ("broker 401
// LAUNCHER_INVALID"). A refusal whose text is withheld names no code, and cipheraddr.WordsNotPrinted
// for its words. The code and the words are the broker's, so each prints as it prints itself
// (redact.Field): ordinary words as they came, a URL with no user information and no query or
// fragment value, redact.WordsNotReadable in place of a field the rule cannot read, and on one
// line, Go-quoted where a line break or an escape sequence would start a line of its own where
// this error prints.
func (e *BrokerError) Error() string {
	s := fmt.Sprintf("broker %d", e.Status)
	if !e.code.IsEmpty() && !e.withheld {
		s += " " + e.code.String()
	}
	switch {
	case e.withheld:
		s += ": " + cipheraddr.WordsNotPrinted
	case !e.said.IsEmpty():
		s += ": " + e.said.String()
	}
	return s
}

// Format prints Error under the verb, flags, width and precision it is given, as fmt prints that
// string, so no verb prints this refusal's fields.
func (e *BrokerError) Format(f fmt.State, verb rune) {
	fmt.Fprintf(f, fmt.FormatString(f, verb), e.Error())
}

// LogValue is Error, so a structured log field names the refusal only as a message may.
func (e *BrokerError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// HasCode reports whether the broker answered code, compared as it came. It answers no text.
func (e *BrokerError) HasCode(code string) bool { return e.code.Equal(code) }

// says reports whether the broker's words hold phrase, compared as they came.
func (e *BrokerError) says(phrase string) bool { return e.said.Contains(phrase) }

// readOperator reads OperatorFile, trimmed, per call: it names the login_hint for a machine
// login, and the "operator" of an enrolment signed with the credential such a login minted,
// which is the one launcher credential of the three the helper can hold whose operator it
// learns from this file (enrolAuthority). Its error names the file as operatorFileNamed.
func (b *Broker) readOperator() (string, error) {
	data, err := os.ReadFile(b.OperatorFile)
	if err != nil {
		return "", fileError("read", operatorFileNamed, err)
	}
	operator := strings.TrimSpace(string(data))
	if operator == "" {
		return "", fmt.Errorf("%s is empty", operatorFileNamed)
	}
	return operator, nil
}

// Operator reads OperatorFile for display only (the "operator" field on `sessions` and
// `register` socket responses): it returns "" on any read failure rather than an error, since
// this field is cosmetic and never security-relevant — unlike readOperator, whose callers use
// its value to build authenticated broker requests and so must see the error.
func (b *Broker) Operator() string {
	operator, _ := b.readOperator()
	return operator
}

// oauthConfig is the OAuth client the helper logs a machine in and refreshes its login with: the
// broker's device authorization and token endpoints, each joined onto the address
// (cipheraddr.Address.Join, the address's path and query kept), and the client's id sent in the
// form. An address that is no absolute URL with a host is refused by its class, "not a valid URL"
// (redact.ClassNotAURL), which prints no part of it.
func (b *Broker) oauthConfig() (*oauth2.Config, error) {
	deviceAuth, err := b.URL.Join("/v1/oauth/device_authorization")
	if err != nil {
		return nil, redact.Masked(err, redact.ClassNotAURL)
	}
	token, err := b.URL.Join("/v1/oauth/token")
	if err != nil {
		return nil, redact.Masked(err, redact.ClassNotAURL)
	}
	return &oauth2.Config{
		ClientID: oauthwire.ClientID, Scopes: []string{oauthwire.Scope},
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: deviceAuth.String(),
			TokenURL:      token.String(),
			AuthStyle:     oauth2.AuthStyleInParams,
		},
	}, nil
}

// request is a request of method to path at the broker, path joined onto the address
// (cipheraddr.Address.Join, the address's path and query kept ahead of path's), and the URL a proof
// of it signs (proof.TargetURI), which the broker checks against its public URL and the path it was
// asked. An address that is no absolute URL with a host is refused by its class, "not a valid URL"
// (redact.ClassNotAURL), and a path no URL can hold (an id a caller gave holding a '#', a bad
// escape or a control byte) as errPathUnsendable; neither prints any part of the address or the
// path.
func (b *Broker) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, string, error) {
	target, err := b.URL.Join(path)
	switch {
	case errors.Is(err, urljoin.ErrRef):
		return nil, "", errPathUnsendable
	case err != nil:
		return nil, "", redact.Masked(err, redact.ClassNotAURL)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, "", b.URL.Error(err)
	}
	return req, proof.TargetURI(target), nil
}

// errPathUnsendable is request's refusal of a path no URL can hold: one that ends in an id a caller
// gave (`unenroll --enrollment`) holding a '#', which net/http would cut off as a fragment and send
// the rest, a bad escape or a control byte. The path is not printed, since that id can be what a
// person typed.
var errPathUnsendable = errors.New("the request's path is not one a URL can hold: the id it ends in holds a '#', a bad escape or a control byte (the id is not printed)")

// audience is the audience of a request object the helper signs for its broker (proof.Audience):
// the URL the broker verifies the object's aud against, with none of the query, fragment or user
// information the address may carry. An address that is no absolute URL with a host is refused by
// its class, "not a valid URL" (redact.ClassNotAURL), which prints no part of it.
func (b *Broker) audience() (string, error) {
	audience, err := proof.Audience(b.URL)
	if err != nil {
		return "", redact.Masked(err, redact.ClassNotAURL)
	}
	return audience, nil
}

// httpClient is b.HTTP, or http.DefaultClient when it is nil, as net/http reads a nil client, with
// redirects refused (redirects.Refused): every call the helper makes to the broker carries a
// proof, a request object, a device code or a refresh token, and a 3xx answered for the broker
// must not hand any of them to a host the deployment never named. It is a copy of its own each
// call, so the caller may change it.
func (b *Broker) httpClient() *http.Client {
	return redirects.Refused(cmp.Or(b.HTTP, http.DefaultClient))
}

// baseTransport is httpClient's transport, or http.DefaultTransport when it is nil, as net/http
// reads a nil transport.
func (b *Broker) baseTransport() http.RoundTripper {
	if t := b.httpClient().Transport; t != nil {
		return t
	}
	return http.DefaultTransport
}

// contextClient is a copy of httpClient, with every field kept, whose transport sends each
// request under ctx. It is for a library that builds its request with no context: x/oauth2
// v0.37.0's DeviceAuth reads its context only to choose the client, so without this the op's
// bound would not end the device authorization and the op's trace would not see it.
func (b *Broker) contextClient(ctx context.Context) *http.Client {
	client := *b.httpClient()
	client.Transport = contextTransport{ctx: ctx, base: b.baseTransport()}
	return &client
}

// contextTransport sends each request through base under ctx (contextClient).
type contextTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t contextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req.WithContext(t.ctx))
}

// tokenClient is the client every token request of a login made with key is sent with: b.HTTP's
// transport and timeout, with a DPoP proof of key on every request, as the broker's token endpoint
// requires, and with redirects refused, as httpClient's are. x/oauth2 reads it from tokenContext's
// context (golang.org/x/oauth2 v0.37.0, internal/transport.go: ContextClient), and pollLogin sends
// its polls with it directly (devicePoll). Each of those puts it under a RefusalClient, as every
// client whose answers the boundary reads is (redact.OAuth2).
func (b *Broker) tokenClient(key *ecdsa.PrivateKey) *http.Client {
	client := b.httpClient()
	client.Transport = cipherclient.NewDPoPTransport(key, b.baseTransport())
	return client
}

// tokenContext is ctx carrying tokenClient's client, the one x/oauth2 sends token requests with,
// under a RefusalClient.
func (b *Broker) tokenContext(ctx context.Context, key *ecdsa.PrivateKey) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, redact.RefusalClient(b.tokenClient(key)))
}

// oauthBrokerError is err, from an OAuth call to the broker at addr, as a message may print it
// (addr.Error), and as a BrokerError when the broker answered it with an OAuth error the redactor
// printed as the answer: the device authorization endpoint's own refusals (invalid_request,
// described "<CODE>: <message>") carry the broker's code and message, any other OAuth error its
// error code and description. The BrokerError holds them as redact.RemoteText, split before they
// are held: the answer redact.OAuth2 made of the broker's (redact.OAuthAnswer.Refusal), or, for an
// answer that never crossed that boundary, its own fields (redact.NewOAuthRefusal). It prints them
// as the rule for remote text reads them.
//
// An answer the redactor did not print as the answer - every answer beside an address that
// carries a credential, which the broker or anyone answering for it can repeat back, one with an
// error_uri, and one with no error code - is still the broker's refusal, a BrokerError of its
// status alone: no code and no word of its description, which are the very text the redactor
// withheld, so no BrokerError holds a part of the credential either. Its words read
// cipheraddr.WordsNotPrinted beside an address that is not clean, and are empty beside a clean
// one, so a machine login's device authorization prints it as "broker <n>" and those words
// (BrokerError.Error). requestReach.settle reads its status, so a 4xx refusal but a 408 is
// reported as one and not as an unknown outcome, and any other status, a 408, a 3xx or a 5xx,
// stays unknown (settle says why); an answer with no status stays as the redactor printed it. It
// reads the broker's own answer (redact.BrokerAnswer), never errors.As, which also reaches an
// answer Class or Masked holds: another server's, whose words they withheld; and it takes
// redact.OAuth2's split of the answer only from the OAuthAnswer that holds that same answer.
//
// The redactor printed the answer exactly when its Cause, which is what it prints of the failure
// without a call's operation and URL, is the answer's text as the rule reads it
// (redact.AnswerText), not the answer's raw text (its Error). That holds under a caller's %w too,
// whose words it drops while it still prints the answer, which is why this asks the text and not
// whether Error answered err itself.
func oauthBrokerError(err error, addr cipheraddr.Address) error {
	shown := addr.Error(err)
	re, ok := redact.BrokerAnswer(err)
	if !ok {
		return shown
	}
	status := 0
	if re.Response != nil {
		status = re.Response.StatusCode
	}
	if addr.Cause(err).Error() != redact.AnswerText(re) {
		if status == 0 {
			return shown
		}
		return newBrokerError(status, redact.RemoteText{}, redact.RemoteText{}, !addr.Clean())
	}
	code, message := redact.NewOAuthRefusal(re.ErrorCode, re.ErrorDescription)
	var answer *redact.OAuthAnswer
	if errors.As(err, &answer) {
		if held, _ := answer.Unwrap().(*oauth2.RetrieveError); held == re {
			code, message = answer.Refusal()
		}
	}
	return newBrokerError(status, code, message, false)
}

// machineFileFrom is the machine login a token answer issues for key: the refresh token and the
// broker's extras naming the launcher credential and when it lapses without a refresh.
func machineFileFrom(key *ecdsa.PrivateKey, tok *oauth2.Token) (*MachineFile, error) {
	id, _ := tok.Extra(oauthwire.ExtraCredentialID).(string)
	raw, _ := tok.Extra(oauthwire.ExtraCredentialExpiresAt).(string)
	expiresAt, err := time.Parse(time.RFC3339, raw)
	if id == "" || err != nil || tok.RefreshToken == "" {
		return nil, fmt.Errorf("the broker's token answer lacks a %s, an RFC 3339 %s or a refresh_token", oauthwire.ExtraCredentialID, oauthwire.ExtraCredentialExpiresAt)
	}
	return &MachineFile{Key: key, CredentialID: id, ExpiresAt: expiresAt.UTC(), RefreshToken: tok.RefreshToken}, nil
}

// Login starts (or reports the running) machine login, an OAuth device grant: a fresh key, signed
// as a launcher_credential request object naming hostname and the OperatorFile's login as
// login_hint, sent to the device authorization endpoint with the key's thumbprint (dpop_jkt). It
// returns the user code for the CLI to print (login-status carries the answer's verification_uri
// beside it while the login is pending), and a background goroutine polls the token endpoint
// until a human decides it (pollLogin), keeping and installing the login on approval or recording
// denial/expiry. It never retries past a terminal state, and a later Login while one is still
// pending just reports it. A login while a credential is held mints a new key and a new login,
// which replaces the held one only once it is issued.
//
// The device authorization is sent under ctx (contextClient), so the op's bound ends it, and its
// failure is settled as known or unknown (requestReach.settle): an answer with no device_code or
// user_code is a 2xx the helper could not read, so it is unknown too. Its interval and lifetime
// are held to the device grant's bounds (cipherclient.BoundDeviceAuth) before the poll starts. A
// code that lapses no later than its first poll, the broker's interval on, could never be polled,
// so Login refuses it at once, before any code is returned (unpollableCode), rather than wait the
// code out and record it expired; a code that has already lapsed is refused so too. The poll
// keeps the client it always had, with no bound: it runs after the op has answered.
func (b *Broker) Login(ctx context.Context, hostname string) (string, error) {
	b.loginMu.Lock()
	defer b.loginMu.Unlock()
	if ls := b.login.Load(); ls != nil && ls.State == "pending" {
		return ls.Code, nil
	}
	key, err := proof.NewKey()
	if err != nil {
		return "", err
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		return "", err
	}
	operator, err := b.readOperator()
	if err != nil {
		return "", err
	}
	audience, err := b.audience()
	if err != nil {
		return "", err
	}
	details := []record.AuthorizationDetail{{Type: record.KindLauncherCredential, Identifier: hostname}}
	compact, err := record.Sign(key, audience, details, "", operator, nil, time.Now())
	if err != nil {
		return "", err
	}
	cfg, err := b.oauthConfig()
	if err != nil {
		return "", err
	}
	ctx, reach := traceReach(ctx)
	// Read before x/oauth2 reads its own start of the request, from which it dates the code's end.
	asked := b.loginNow()
	da, err := redact.OAuth2(cfg.DeviceAuth(context.WithValue(ctx, oauth2.HTTPClient, redact.RefusalClient(b.contextClient(ctx))),
		oauth2.SetAuthURLParam("request", compact), oauth2.SetAuthURLParam("dpop_jkt", thumbprint)))
	if err != nil {
		return "", reach.settle(oauthBrokerError(err, b.URL))
	}
	if da.DeviceCode == "" || da.UserCode == "" {
		return "", reach.settle(errors.New("broker returned no device_code/user_code"))
	}
	named := da.Interval
	hourEnds := cipherclient.BoundDeviceAuth(da, b.loginNow())
	if left := da.Expiry.Sub(b.loginNow()); time.Duration(da.Interval)*time.Second >= left {
		return "", unpollableCode(da.Expiry.Sub(asked), left, named, da.Interval, hourEnds)
	}
	b.recordLogin(&loginState{Code: da.UserCode, State: "pending", VerificationURI: da.VerificationURI})
	go b.pollLogin(key, cfg, da, operator)
	return da.UserCode, nil
}

// loginNow is the clock Login reads around the device authorization's request: time.Now, or in a
// test the moments testLoginNow hands it.
func (b *Broker) loginNow() time.Time {
	if b.testLoginNow != nil {
		return b.testLoginNow()
	}
	return time.Now()
}

// unpollableCode is Login's refusal of a device code that lapses, left from now, no later than
// its first poll, used seconds on (a poll waits the interval first, RFC 8628 section 3.5): no poll
// could reach it, and an approval of it would mint a launcher credential no poll collects. lived
// is the code's end less the moment Login asked for it: x/oauth2 dates the end from its request,
// less the round trip, so lived is the broker's lifetime and lived less left the round trip. The
// line names the interval the broker named, with the one used where the bounds set it, and what
// ends the code: the hour this machine polls a code at most, where hourEnds
// (cipherclient.BoundDeviceAuth) or where the interval used is the hour and the broker's lifetime
// longer; a lifetime that had passed when the answer came, a negative one or one x/oauth2 wraps
// into the past, as it does one of about 292 to 584 years; the round trip, where the broker's
// lifetime alone outlives an interval under the hour; or else that lifetime, and that the round
// trip outlasted it where it did. It names what must change at the broker, worded from that cause
// (unpollableRemedy), not asking again, which would meet the same answer, but for the round trip,
// which a faster answer cures, and where the answer outlasted the code, after which the codes must
// also outlive the interval by more than the answers take.
//
// lived is the broker's whole seconds only as far as a clock reading is on time: x/oauth2's type
// holds the code's end and no raw expires_in, and one it wraps forward keeps a fraction of a
// second that no reading of lived tells from a pause. So a line never names a cure the code's own
// end may already meet: where it names a lifetime the interval is no shorter than, it says the
// codes must also outlive the interval by more than the answers take (transit), which holds
// whatever lived holds, and a cure by the answer's speed names no more time than lived leaves, to
// a millisecond (the whole seconds, or the time lived leaves where that is less). A code that had
// lapsed by the time its answer arrived meets no cure by a lifetime: its end is past.
func unpollableCode(lived, left time.Duration, named, used int64, hourEnds bool) error {
	var poll string
	switch {
	case named == 0:
		// x/oauth2 reads an interval of 0 as none.
		poll = fmt.Sprintf("no poll interval or one of 0 s, so one of %d s", used)
	case named != used:
		poll = fmt.Sprintf("a poll interval of %d s, so one of %d s", named, used)
	default:
		poll = fmt.Sprintf("a poll interval of %d s", named)
	}
	const lapses = "the code would lapse before its first poll"
	const verdict = "no machine login works at that broker until"
	// What a login needs of the codes once their lifetime outlasts the interval: true whatever
	// lived hides, since a code is polled only where lived less the round trip is more than the
	// interval.
	const outlive = "once its codes outlive its poll interval by more than its answers take to arrive"
	// The same, added where the interval is no shorter than the lifetime, a cure the code's own
	// end may already meet (see above).
	const transit = ", and then only " + outlive
	// The broker's whole seconds, as far as lived says: it is off them by the moments between
	// Login's and x/oauth2's clock readings, far under half a second, and by the fraction of a
	// lifetime x/oauth2 wraps forward, which this cannot tell from a pause (see above).
	lifetime := lived.Round(time.Second)
	interval := time.Duration(used) * time.Second
	if hourEnds || (used == cipherclient.MostDeviceInterval && lifetime > interval) {
		return fmt.Errorf("the broker answered %s, and this machine polls a device code for an hour at most: %s; %s its poll interval is shorter than an hour", poll, lapses, verdict)
	}
	// The end came no later than Login asked: a negative lifetime, or one x/oauth2 wrapped into the
	// past. One it wraps forward to under half a second rounds to 0 s but has not passed.
	if lived <= 0 {
		return fmt.Errorf("the broker answered a device code that had lapsed by the time its answer arrived (a negative lifetime, or one this machine's OAuth library wraps into the past, as it does one of about 292 to 584 years), with %s; %s %s, and this machine's OAuth library does not wrap their lifetime into the past, as it wraps none under about 292 years", poll, verdict, unpollableRemedy(named, used, lifetime))
	}
	lives := fmt.Sprintf("the broker answered a device code that lives %d s, with %s", lifetime/time.Second, poll)
	if interval >= lifetime && left > 0 {
		return fmt.Errorf("%s: %s; %s %s%s", lives, lapses, verdict, unpollableRemedy(named, used, lifetime), transit)
	}
	// The round trip took what the broker's numbers left, or the answer outlasted the code.
	trip := lived - left
	took := fmt.Sprintf("%s, but its answer took %.3f s to arrive", lives, trip.Seconds())
	if interval >= lifetime {
		return fmt.Errorf("%s, so the code had lapsed; %s %s%s", took, verdict, unpollableRemedy(named, used, lifetime), transit)
	}
	rest := "none of it, so the code had lapsed"
	if left > 0 {
		rest = fmt.Sprintf("%.3f s of it: %s", left.Seconds(), lapses)
	}
	// An answer in less than the time the code's end leaves over the interval works: lived less the
	// interval, to the millisecond, or the lifetime's whole seconds over it where lived is within a
	// millisecond of holding as many (a lifetime x/oauth2 wraps forward by half a second or more
	// reads as the next whole second, which lived does not hold). So the line names at most a
	// millisecond more than lived holds, and lived is the broker's whole seconds less the gap
	// between x/oauth2's own two clock readings, which no bound limits (a pause between them is
	// the shortfall the unit rows pin): the figure can read a little under the whole seconds, as
	// "less than 0.998 s" for a 6 s code, true and that much conservative. The millisecond of
	// tolerance only absorbs readings a microsecond or so apart.
	over := lifetime - interval
	within := fmt.Sprintf("%d s", over/time.Second)
	if held := (lived - interval).Truncate(time.Millisecond); held < over-time.Millisecond {
		within = fmt.Sprintf("%.3f s", held.Seconds())
	}
	return fmt.Errorf("%s, which left %s; a machine login works at that broker where its answer arrives in less than %s, or "+outlive, took, rest, within)
}

// unpollableRemedy is what must change at the broker before a code it answers can be polled, for a
// broker that named the interval named (0 for none, as x/oauth2 reads it), polled at used, and a
// code whose lifetime, as the broker named it, is lifetime, no longer than used seconds: 0 or less
// where it is negative or x/oauth2 wrapped it into the past, and 0 where x/oauth2 wrapped it
// forward to under half a second. This machine polls at the interval named,
// cipherclient.LeastDeviceInterval at least and cipherclient.MostDeviceInterval, the hour, at most
// (cipherclient.BoundDeviceAuth), so where the interval used is the least, or the code lives that
// or less, no shorter interval alone helps: the code must live longer, and where the interval used
// is the most, the interval must also be shorter than the hour.
func unpollableRemedy(named, used int64, lifetime time.Duration) string {
	switch {
	case named == 0 && lifetime > cipherclient.LeastDeviceInterval*time.Second:
		return fmt.Sprintf("it names a poll interval shorter than its codes' lifetime, or its codes live longer than %d s", used)
	case named == 0:
		return fmt.Sprintf("its codes live longer than %d s, or than both %d s and a poll interval it names", used, cipherclient.LeastDeviceInterval)
	case used == cipherclient.LeastDeviceInterval:
		return fmt.Sprintf("its codes live longer than %d s, the least poll interval this machine keeps", used)
	case lifetime > cipherclient.LeastDeviceInterval*time.Second:
		return "its poll interval is shorter than its codes' lifetime"
	case used == cipherclient.MostDeviceInterval:
		return "its poll interval is shorter than an hour and its codes live longer than it"
	default:
		return "its codes live longer than its poll interval"
	}
}

// LoginStatus reports the most recent machine login, current or settled, whether the helper holds
// a launcher credential and when that credential lapses without a refresh; the zero value means no
// login has run or been resumed. A login that did not issue says nothing about the credential an
// earlier one installed: CredentialHeld does. Once the helper has dropped the credential (drop: the
// broker refused it or its refresh, or it reached its expiry), and until a login is recorded (one
// starts, or one pending at the drop settles), every state but pending reads "expired", refused and
// why it was dropped; an issued login is among them, since only drop ever clears a credential an
// issued login installed. The login, the credential and the drop are read under stateMu, so the
// answer is all three of one moment.
func (b *Broker) LoginStatus() loginState {
	b.stateMu.Lock()
	ls, cred, dropped := b.login.Load(), b.cred.Load(), b.dropped
	b.stateMu.Unlock()
	if ls == nil {
		return loginState{}
	}
	out := *ls
	out.CredentialHeld = cred != nil
	if cred != nil {
		out.CredentialExpiresAt = cred.expiresAt
	}
	if !out.CredentialHeld && dropped != "" {
		out.Refused, out.Dropped = true, dropped
		if out.State != "pending" {
			// The drop is newer than the login, so its cause, not the login's, is why it reads so.
			out.State, out.Reason = "expired", ""
		}
	}
	return out
}

// noCredentialReason says why the helper holds no launcher credential, for the error lines that
// say a session cannot enroll: NoCredentialReason of this moment's login-status.
func (b *Broker) noCredentialReason() string {
	ls := b.LoginStatus()
	return NoCredentialReason(ls.State, ls.Reason, ls.Refused, ls.Dropped)
}

// recordLogin records ls as the current machine login. Whether the login starts or settles, its
// state is newer than any drop recorded before it, so the same write ends that record and the
// login reports its own state.
func (b *Broker) recordLogin(ls *loginState) {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	b.login.Store(ls)
	b.dropped = ""
}

// HasCredential reports whether the helper holds a launcher credential, the one thing every
// enrollment needs.
func (b *Broker) HasCredential() bool {
	return b.cred.Load() != nil
}

// CredentialInstalled returns a channel that closes the next time a machine login installs a
// credential, so a retry that failed for want of one can try again at once rather than when its
// backoff comes round.
func (b *Broker) CredentialInstalled() <-chan struct{} {
	b.installedMu.Lock()
	defer b.installedMu.Unlock()
	if b.installed == nil {
		b.installed = make(chan struct{})
	}
	return b.installed
}

// installCredential makes cred the helper's launcher credential and issued the login that minted
// it, in one write under stateMu that also ends any record of a dropped credential, then wakes
// every waiter on CredentialInstalled, watches cred's expiry (watchExpiry) and has the refresh
// loop schedule its next refresh from cred's expiry (nextRefresh). It returns the credential it
// replaced, if any, which is gone from then on, so its expiry watch says nothing more about it.
func (b *Broker) installCredential(cred *machineCredential, issued *loginState) *machineCredential {
	b.stateMu.Lock()
	old := b.cred.Swap(cred)
	b.login.Store(issued)
	b.dropped = ""
	b.refusedCred = nil
	b.stateMu.Unlock()
	if old != nil {
		close(old.gone)
	}
	go b.watchExpiry(cred)
	select {
	case b.refreshSignals().reschedule <- struct{}{}:
	default:
	}
	b.installedMu.Lock()
	defer b.installedMu.Unlock()
	if b.installed != nil {
		close(b.installed)
	}
	b.installed = make(chan struct{})
	return old
}

// pollLogin polls the token endpoint for a pending machine login until a human decides it, one
// token request per poll (devicePoll) through the client tokenClient makes, the one x/oauth2 reads
// from tokenContext's context for every other token request. On approval it keeps the login
// (keepLogin). access_denied records "denied"; expired_token, a device code the broker no longer
// knows (invalid_grant) and the code's own lifetime passing record "expired", each error only
// from a refusal, a 4xx that is no outage (oauthrefusal.Refusal). The status alone decides
// refused or down: a poll that does not reach the broker, or that it answers a 5xx, a 408 or a 429
// (oauthrefusal.Outage) naming one of those errors included, is the broker down and polls again; a
// refusal naming no error or another one is the broker answering with no decision, and polls
// again too, until the code's lifetime records "expired".
//
// The poll starts at the broker's interval and the code lives as long as the broker says, each
// held to the device grant's bounds, which Login applied (cipherclient.BoundDeviceAuth), and Login
// starts no poll that could not come before the code lapses. Each poll waits the interval first,
// and RFC 8628 section 3.5 grows it by 5 s for that poll and every later one on a slow_down, at
// whatever status the body names it, and on a poll the broker did not answer or answered with an
// outage, once for an answer that is both. The Retry-After of an outage the
// broker answered, a 408, a 429 or a 5xx, in seconds or as an HTTP date (serverwait.Read), is
// waited where it asks longer, though never past the code's lifetime, which ends any wait (the
// poll's context). authorization_pending keeps the interval. A lapse after polls the broker did not
// answer or answered with an outage records, in the "expired" state's Reason, how many came in a
// row (pollsUnreached); the lapse is no poll.
//
// These are the TypeScript device login's interval and Retry-After rules, with its bounds
// (packages/cipher-client/src/device-login.ts, finishDeviceLogin). The two differ by choice on a
// refusal naming no deciding error: there it stops the poll, where a person waits on it; here the
// helper polls on in the background. They also read apart some answers no conforming broker gives,
// where their libraries or their own checks differ. This is the one list of them, which
// startDeviceLogin's and finishDeviceLogin's docs point to; it is not complete:
//   - An OAuth error counts here in a form or text body and beside a WWW-Authenticate challenge,
//     as x/oauth2 reads it (devicePoll); there the library reads one only from a JSON 4xx with no
//     challenge, and stops at any other refusal, naming its status.
//   - A 2xx naming an OAuth error is the broker's answer here, as x/oauth2 reads it; there a 200
//     naming one is an outage where it holds no token and the approval where it holds one, and
//     any other 2xx stops the poll.
//   - Here a 2xx with a token in a JSON body, or in a form or text body read as a form, is the
//     approval, another 2xx an outage, and a 3xx an answer, polled on; there the library takes
//     a 200 body that parses as JSON, whatever its type, the login keeps it only where the
//     library's and its own checks pass (loginOf), and the poll stops at a 3xx and at any other
//     2xx.
//   - A poll still in flight at the code's end is cut off here (its context), and finishes there.
//   - serverwait.Read here also reads a Retry-After count signed "+", which there is no wait, and a
//     Retry-After date counts there in any form Date.parse reads. The poll's refusal crosses the
//     boundary (redact.OAuth2), which keeps no header of it but Retry-After, so here, as there,
//     X-RateLimit-Reset is no wait.
//   - The device authorization's edge values: here they read as cipherclient.BoundDeviceAuth's doc
//     says; there oauth4webapi 3.8.8 (build/index.js:2185-2197) fails the login on an interval
//     that is null, 0 or negative and on a lifetime that is missing or negative, and keeps a
//     fraction of either, a count past an int64 or a lifetime sent as a string; the login holds
//     the interval to 1 s at least, with no most, and the lifetime to an hour. Each client refuses
//     a code no poll could reach before its code is shown, so an interval past what a
//     time.Duration holds, kept there and held to an hour here, is refused in both. A lifetime of
//     0 is a code already lapsed there, refused so; here it is none, polled for the hour. A
//     lifetime past about 584 years can wrap here, as x/oauth2 counts it, to one under the hour,
//     which this helper takes for the broker's: 18446744974 s comes out as about 900 s, polled
//     for that against a shorter interval and refused against a longer one, as against 1000 s,
//     where the line names a code that lives about 900 s and asks for an interval shorter than its
//     codes' lifetime, and then that the codes must outlive the interval by more than the answers
//     take; there the hour holds it, and a 1000 s interval is polled. x/oauth2's type keeps no raw
//     expires_in, so this helper cannot read what the broker sent: a lifetime it wraps forward keeps
//     a fraction that no reading tells from a pause, as 18446744075 s comes out as 1.29 s, polled
//     where its answer arrives in under 0.29 s and refused, as "lives 1 s", where it takes longer.
//     So a line that names a lifetime the interval is no shorter than never names a cure by the
//     interval or the lifetime alone: it adds that the codes must outlive the interval by more
//     than the answers take, which holds of a wrapped code as of any. Here the code's end is
//     dated less the device authorization's round trip, as x/oauth2 dates it, and there
//     from when the answer arrived, so on a slow path this helper refuses an answer that login
//     shows: an interval of 599 s against a 600 s code, with a round trip over a second. There the
//     test counts the poll's wait up and the code's end down to a whole millisecond, so it refuses
//     an interval less than two milliseconds shorter than its code, as 5 s against 5.0005 s or
//     4.9997 s against 5 s, where this helper reads only whole seconds.
func (b *Broker) pollLogin(key *ecdsa.PrivateKey, cfg *oauth2.Config, da *oauth2.DeviceAuthResponse, operator string) {
	client := b.tokenClient(key)
	ctx, cancel := context.WithDeadline(context.Background(), da.Expiry)
	defer cancel()
	settle := func(state, reason string) {
		b.recordLogin(&loginState{Code: da.UserCode, State: state, Reason: reason})
	}
	interval := time.Duration(da.Interval) * time.Second
	wait := interval
	// unreached counts the polls in a row the broker did not answer or answered with an outage.
	unreached := 0
	for {
		if left := time.Until(da.Expiry); left > 0 {
			b.loginWait(ctx, min(wait, left), da)
		}
		// The code's end alone says it lapsed: Login made it one monotonic reading
		// (cipherclient.BoundDeviceAuth), the context's deadline, so the two read one clock, and a
		// test may only bring the end nearer.
		if time.Until(da.Expiry) <= 0 {
			settle("expired", pollsUnreached(unreached))
			return
		}
		tok, err := devicePoll(ctx, client, cfg, da)
		if err == nil {
			b.keepLogin(key, tok, da.UserCode, operator)
			return
		}
		switch oauthrefusal.Refusal(err) {
		case redact.AccessDenied:
			settle("denied", "")
			return
		case redact.ExpiredToken, redact.InvalidGrant:
			settle("expired", "")
			return
		}
		if ctx.Err() != nil {
			// The code's lifetime cut this poll off: the lapse, no poll the broker did not answer.
			continue
		}
		var answered *oauth2.RetrieveError
		outage := !errors.As(err, &answered) || oauthrefusal.Outage(answered.Response.StatusCode)
		if outage {
			unreached++
		} else {
			unreached = 0
		}
		if outage || redact.RefusalCode(err) == redact.SlowDown {
			interval += deviceIntervalStep
		}
		// The top of the loop holds the wait to what is left of the code's lifetime.
		wait = interval
		if outage {
			wait = max(interval, retryAfter(err))
		}
	}
}

// loginWait waits d before pollLogin's next poll, or until ctx ends (the code's lifetime passing),
// or, in a test, hands d and the device authorization to testLoginWait and returns at once.
func (b *Broker) loginWait(ctx context.Context, d time.Duration, da *oauth2.DeviceAuthResponse) {
	if b.testLoginWait != nil {
		b.testLoginWait(d, da)
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// devicePoll is one poll of da's device code (RFC 8628 section 3.4) through client, under ctx,
// read as x/oauth2 reads a token endpoint's answer (golang.org/x/oauth2 v0.37.0,
// internal/token.go:258-333): a 2xx whose JSON, form or text body holds an access_token and names
// no error is the token, its extras kept (Token.Extra). Any other status, and a 2xx naming an
// error, is the broker's answer carrying the error its body names, which oauthrefusal reads. A
// 2xx whose body does not parse, or holds no access_token, is a plain error, no token endpoint's
// answer, which pollLogin counts as an outage, as it counts no answer, or one cut off before its
// body was read whole (the transport's error). It reads expires_in apart from x/oauth2: here a
// fraction, or a count that is not positive, leaves the token no expiry, which nothing in a login
// reads (machineFileFrom), and a large count is not held. x/oauth2 dates a negative count in the
// past, holds a count in a JSON body to an int32, and refuses a JSON body whose expires_in is a
// fraction, an exponent or past an int64, reading neither its token nor its error, where
// devicePoll reads both: a 403 access_denied with such an expires_in is a denial here.
//
// The poll goes out under a RefusalClient over client, so every answer and every failed hop
// carries the record of the requests it came of. A poll that got no answer prints as
// redact.Unanswered names it: net/http's operation, the URL the poll asked (its scheme, host and
// path, never the user information or query the configured broker URL can carry) and the
// failure's own transport words or its class, never net/http's text, which quotes that URL whole.
// A token endpoint URL net/url refuses, which the address's join can still write (Broker.request's
// reparsed address), prints that way too: the class "not a valid URL" and no part of the URL,
// which net/url's own error quotes whole, user information, query and all.
// An answer is read through redact.ReadAnswer, as far as the limit where it runs past it, as the
// limit always cut it. A JSON body is decoded through redact.Answer.Decode, with the extras
// machineFileFrom reads (credential_id and credential_expires_at) as the token's; a form or text
// body is read from the bytes Answer.StreamTo hands it, as x/oauth2 reads them. The broker's
// error code and description are held as redact.RemoteText with the answer's record
// (Answer.Said) as they are read, so no word of the answer is ever kept as it came, and the
// refusal is x/oauth2's own type holding the rule's reading of those two alone
// (redact.NewRetrieveError) over the answer itself, which the boundary (redact.OAuth2) reads with
// the poll's record and then rebuilds: what the caller sees holds the status with its standard
// phrase, the Retry-After the answer named and the rule's reading of the code and the
// description, and no other header, no status line the broker wrote and no body. Its code is read
// as it came (redact.RefusalCode), never from the ErrorCode it holds. An answer's error_uri is
// read nowhere: x/oauth2 prints it beside the code and the description, and the refusal this
// builds carries those two alone.
func devicePoll(ctx context.Context, client *http.Client, cfg *oauth2.Config, da *oauth2.DeviceAuthResponse) (*oauth2.Token, error) {
	form := url.Values{
		"client_id":   {cfg.ClientID},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {da.DeviceCode},
	}
	if len(cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, redact.Unanswered(err, nil, "")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := redact.RefusalClient(client).Do(req)
	if err != nil {
		return nil, redact.Unanswered(err, req, "")
	}
	defer resp.Body.Close()
	read, err := redact.ReadAnswer(resp, 1<<20)
	if err != nil && !errors.Is(err, redact.ErrAnswerTooLarge) {
		return nil, fmt.Errorf("read the device code poll's answer: %w", err)
	}
	var body bytes.Buffer
	_, _ = read.StreamTo(&body)
	// code and description stay empty where the body does not parse or names no error.
	var code, description redact.RemoteText
	// tok stays nil where the body does not parse.
	var tok *oauth2.Token
	if content, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); content == "application/x-www-form-urlencoded" || content == "text/plain" {
		if vals, err := url.ParseQuery(body.String()); err == nil {
			code, description = read.Said(vals.Get("error")), read.Said(vals.Get("error_description"))
			tok = (&oauth2.Token{AccessToken: vals.Get("access_token"), TokenType: vals.Get("token_type"), RefreshToken: vals.Get("refresh_token")}).WithExtra(vals)
			if seconds, _ := strconv.Atoi(vals.Get("expires_in")); seconds > 0 {
				tok.Expiry = time.Now().Add(time.Duration(seconds) * time.Second)
			}
		}
	} else {
		var fields struct {
			AccessToken         string      `json:"access_token"`
			TokenType           string      `json:"token_type"`
			RefreshToken        string      `json:"refresh_token"`
			ExpiresIn           json.Number `json:"expires_in"`
			CredentialID        string      `json:"credential_id"`
			CredentialExpiresAt string      `json:"credential_expires_at"`
			Error               string      `json:"error"`
			Description         string      `json:"error_description"`
		}
		if read.Decode(&fields) == nil {
			code, description = read.Said(fields.Error), read.Said(fields.Description)
			tok = (&oauth2.Token{AccessToken: fields.AccessToken, TokenType: fields.TokenType, RefreshToken: fields.RefreshToken}).WithExtra(map[string]any{
				oauthwire.ExtraCredentialID: fields.CredentialID, oauthwire.ExtraCredentialExpiresAt: fields.CredentialExpiresAt,
			})
			if seconds, _ := fields.ExpiresIn.Int64(); seconds > 0 {
				tok.Expiry, tok.ExpiresIn = time.Now().Add(time.Duration(seconds)*time.Second), seconds
			}
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || !code.IsEmpty() {
		refusal := redact.NewRetrieveError(resp.StatusCode, code, description)
		// The answer itself, for the boundary alone: it reads the record of the poll's requests
		// off it and keeps the Retry-After it parses as, and the answer it hands back holds
		// neither it nor any other header.
		refusal.Response = resp
		return redact.OAuth2((*oauth2.Token)(nil), refusal)
	}
	if tok == nil || tok.AccessToken == "" {
		return nil, errors.New("the broker's 2xx answer to a device code poll holds no access_token")
	}
	return tok, nil
}

// deviceIntervalStep is what a slow_down, or a poll the broker did not answer, adds to a device
// code's interval (pollLogin): the 5 s of RFC 8628 section 3.5.
const deviceIntervalStep = 5 * time.Second

// pollsUnreached is the Reason of a login that expired after n polls in a row the broker did not
// answer or answered with an outage, in the words the TypeScript device login's lapse uses, and ""
// for none.
func pollsUnreached(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return "the broker was unreachable or busy for the last poll"
	}
	return fmt.Sprintf("the broker was unreachable or busy for the last %d polls", n)
}

// keepLogin keeps the login an approved device grant issued at MachineFile and only then installs
// it, under credMu, so no refresh runs between the two; it logs the credential with operator, the
// login_hint the login was signed with, whatever the operator file says by then. A credential the
// new login replaced, or, holding none, the one a 401 LAUNCHER_INVALID dropped since the last
// install (refusedCred), is then retired at the broker (retireReplaced), which leaves the sessions
// it enrolled running. An approval the helper cannot keep (a token answer without the credential's
// extras, or a machine file it cannot write) is logged at ERROR and recorded "expired", leaving
// cred untouched.
func (b *Broker) keepLogin(key *ecdsa.PrivateKey, tok *oauth2.Token, code, operator string) {
	f, err := machineFileFrom(key, tok)
	var replaced *machineCredential
	if err == nil {
		b.credMu.Lock()
		if err = b.saveFile(f); err == nil {
			b.stateMu.Lock()
			refused := b.refusedCred
			b.stateMu.Unlock()
			replaced = b.installCredential(f.credential(), &loginState{Code: code, State: "issued"})
			if replaced == nil {
				replaced = refused
			}
		}
		b.credMu.Unlock()
	}
	if err != nil {
		b.logger().Error("machine login approved, but the helper could not keep it; "+cipherclient.Hint(cipherclient.FailureNoMachineLogin), "error", err)
		b.recordLogin(&loginState{Code: code, State: "expired"})
		return
	}
	b.logger().Info("machine login issued; the helper holds a launcher credential",
		"credential_id", f.CredentialID, "operator", operator, "expires_at", f.ExpiresAt.Format(time.RFC3339))
	if replaced != nil && replaced.id != f.CredentialID {
		b.retireReplaced(replaced)
	}
}

// retireReplaced ends old, the credential a new machine login replaced, at the broker: POST
// /v1/operator/machines/{id}/retire signed with old's own launcher proof. The broker sets its
// expiry to now and deletes its OAuth tokens, so no old copy of the machine file can refresh it or
// enroll with it, and leaves every session it enrolled running: they renew with their own proofs.
// It is best effort: a failure is logged at WARN, never retried, and changes nothing the helper
// holds.
func (b *Broker) retireReplaced(old *machineCredential) {
	err := func() error {
		req, htu, err := b.request(context.Background(), http.MethodPost, "/v1/operator/machines/"+old.id+"/retire", nil)
		if err != nil {
			return err
		}
		compact, err := proof.SignLauncher(old.key, old.id, http.MethodPost, htu, time.Now())
		if err != nil {
			return err
		}
		req.Header.Set("Proof", compact)
		return b.do(req, []int{http.StatusNoContent}, nil)
	}()
	if err != nil {
		b.logger().Warn("the helper could not retire the machine login a new one replaced; it lapses at its expiry, or end it and its sessions with agent-secrets machine revoke",
			"credential_id", old.id, "error", err)
		return
	}
	b.logger().Info("the helper retired the machine login a new one replaced", "credential_id", old.id)
}

// Start resumes the machine login kept at MachineFile, if there is one, and starts the refresh
// loop (refreshLoop) until ctx ends; serve runs it before it listens, holding the machine file's
// claim. It first removes the temporary files an interrupted save left beside the file
// (RemoveLeftoverMachineTemps), logging a failure. A kept login is refreshed once before it is
// installed, within startRefreshBound (5 s), so the socket still listens within `register`'s wait:
// a refresh the broker answers is saved and installed as an issued login with no code; one it
// refuses (invalid_grant: revoked, lapsed or reused) drops the login (dropRefreshRefused) and
// deletes the file, so a login the broker answered as ended is never resumed; one whose outcome
// the helper did not learn (the broker unreachable or rate-limiting, a lost or late answer, a 5xx)
// installs the kept login as issued, though the broker may have ended it, and leaves the refresh
// loop to retry it on the retrySchedule. A file that is there but is not a machine login, or that
// others can read, is an error naming it.
func (b *Broker) Start(ctx context.Context) error {
	if b.Identity != nil {
		return b.startIdentity(ctx)
	}
	if err := RemoveLeftoverMachineTemps(b.MachineFile); err != nil {
		b.logger().Error("removing the temporary files an interrupted machine file save left", "machine_file", MachineFileNamed, "error", err)
	}
	f, err := LoadMachineFile(b.MachineFile)
	sched := b.newRetrySchedule()
	switch {
	case errors.Is(err, os.ErrNotExist):
		go b.refreshLoop(ctx, b.refreshEveryOrDefault(), sched)
		return nil
	case err != nil:
		return err
	}
	kept := f.credential()
	b.credMu.Lock()
	bounded, cancel := context.WithTimeout(ctx, b.startRefreshBoundOrDefault())
	next, refusal, err := b.redeem(bounded, kept)
	cancel()
	first := time.Duration(0)
	switch {
	case refusal != "":
		b.installCredential(kept, &loginState{State: "issued"})
		b.drop(kept, dropRefreshRefused, "code", refusal)
		b.removeRefusedFile()
	case err != nil:
		b.installCredential(kept, &loginState{State: "issued"})
		first = sched.next(err)
	default:
		saveErr := b.saveFile(next)
		if saveErr != nil {
			first = sched.retry
		}
		f = next
		installed := next.credential()
		b.installCredential(installed, &loginState{State: "issued"})
		if saveErr != nil {
			b.unsaveFile(installed, saveErr)
		}
	}
	b.credMu.Unlock()
	if refusal == "" {
		b.logger().Info("the helper resumed the machine login it kept", "credential_id", f.CredentialID,
			"expires_at", f.ExpiresAt.Format(time.RFC3339), "machine_file", MachineFileNamed)
	}
	if first == 0 {
		first = b.nextRefresh(b.refreshEveryOrDefault())
	}
	go b.refreshLoop(ctx, first, sched)
	return nil
}

// errAnswerLacksCredential stops startIdentity's trade loop: the broker served a token, but no
// launcher credential to enrol sessions with. It describes the answer, not the helper, which may
// still hold the credential an earlier trade gave it. It names no URL, so a log line prints it as
// it is.
var errAnswerLacksCredential = fmt.Errorf("the broker's token answer carries no %s or RFC 3339 %s, so it gives this machine no launcher credential", oauthwire.ExtraCredentialID, oauthwire.ExtraCredentialExpiresAt)

const identityShutdownLine = "the helper stopped trading this machine's identity: it is shutting down"

// startIdentity is Start for a machine that proves an identity of its own: the token client
// trades it for one of the broker's access tokens, whose launcher credential the helper then
// enrolls sessions with, and keeps trading before each one lapses (cipherclient.Client.Run) until
// ctx ends. No person is asked and no token or credential is kept on disk: a restart trades
// again, with the key it kept (LoadOrMakeIdentityKey), so that trade moves the credential this
// machine already holds.
//
// It waits startRefreshBound (5 s) for the first credential, so a helper on a reachable broker
// listens with one in hand, well inside the 10 s `agent-secrets register` waits for the socket;
// a broker that is slower or down leaves the helper listening with none, answering NO_CREDENTIAL
// while the loop keeps trading, as it does for a machine whose login has lapsed. A refusal of
// the identity itself stops the loop, logged at ERROR: no retry cures it, and the operator's
// deployment is what has to change.
func (b *Broker) startIdentity(ctx context.Context) error {
	first := make(chan struct{})
	var once sync.Once
	go func() {
		err := b.Identity.Run(ctx, func(_ context.Context, token *cipherclient.Token) error {
			if token.CredentialID == "" || token.CredentialExpiresAt.IsZero() {
				return errAnswerLacksCredential
			}
			cred := &machineCredential{key: b.Identity.Key(), id: token.CredentialID, expiresAt: token.CredentialExpiresAt.UTC(), gone: make(chan struct{})}
			b.credMu.Lock()
			b.installCredential(cred, &loginState{State: "issued"})
			b.credMu.Unlock()
			b.logger().Info("the helper traded this machine's identity for its launcher credential",
				"credential_id", cred.id, "expires_at", cred.expiresAt.Format(time.RFC3339))
			once.Do(func() { close(first) })
			return nil
		})
		if err != nil {
			b.identityStopped.Store(true)
			// The answer that lacks a credential is this helper's own words, naming no URL, so it
			// is logged as it stands; a refusal, or a failure of the call, is redacted.
			reason := b.URL.Error(err)
			if errors.Is(err, errAnswerLacksCredential) {
				reason = err
			}
			b.logger().Error("the helper stopped trading this machine's identity: nothing renews its launcher credential, and no session enrols under this machine's credential once that lapses, until the helper restarts", "error", reason)
		} else {
			b.logger().Info(identityShutdownLine)
		}
		once.Do(func() { close(first) })
	}()
	select {
	case <-first:
	case <-time.After(b.startRefreshBoundOrDefault()):
	case <-ctx.Done():
	}
	return nil
}

func (b *Broker) startRefreshBoundOrDefault() time.Duration {
	if b.startRefreshBound > 0 {
		return b.startRefreshBound
	}
	return defaultStartRefreshBound
}

// refreshSignals wake the refresh loop: reschedule when a credential is installed, so the next
// refresh follows the new credential's expiry (nextRefresh), and refused with a credential a
// launcher call's 401 LAUNCHER_INVALID dropped, so the loop settles its machine file at once
// (settleRefused) rather than at the next scheduled refresh.
type refreshSignals struct {
	reschedule chan struct{}
	refused    chan *machineCredential
}

func (b *Broker) refreshSignals() refreshSignals {
	b.signalsOnce.Do(func() {
		b.signals = refreshSignals{reschedule: make(chan struct{}, 1), refused: make(chan *machineCredential, 1)}
	})
	return b.signals
}

// wakeRefused hands cred, just dropped for a 401 LAUNCHER_INVALID, to the refresh loop; a
// credential handed earlier and not yet taken is replaced, since only the latest drop's file
// matters.
func (b *Broker) wakeRefused(cred *machineCredential) {
	ch := b.refreshSignals().refused
	for {
		select {
		case ch <- cred:
			return
		default:
		}
		select {
		case <-ch:
		default:
		}
	}
}

func (b *Broker) refreshEveryOrDefault() time.Duration {
	if b.refreshEvery == 0 {
		return defaultRefreshEvery
	}
	return b.refreshEvery
}

func (b *Broker) refreshRetryOrDefault() time.Duration {
	if b.refreshRetry == 0 {
		return defaultRefreshRetry
	}
	return b.refreshRetry
}

// newRetrySchedule is the refresh loop's retrySchedule, at refreshRetry and the default backoff.
func (b *Broker) newRetrySchedule() retrySchedule {
	return retrySchedule{retry: b.refreshRetryOrDefault(), backoffStart: defaultRefreshBackoff, backoff: defaultRefreshBackoff}
}

// saveFile writes f to MachineFile (testSave in a test), and writes nothing after Close. Every
// caller holds credMu.
func (b *Broker) saveFile(f *MachineFile) error {
	if b.closed {
		return nil
	}
	if b.testSave != nil {
		return b.testSave(f, b.MachineFile)
	}
	return f.Save(b.MachineFile)
}

// Close waits for whatever holds credMu (a save, or a refresh's token request, which ends with the
// refresh loop's context), then makes every later save of the machine file write nothing, so a
// caller can remove the file's directory while the refresh loop or an approved login's poll is
// still finishing. A test's cleanup calls it; the helper never does, since a refresh answered
// while the helper stops spends the old token, and the new one must reach the file.
func (b *Broker) Close() {
	b.credMu.Lock()
	defer b.credMu.Unlock()
	b.closed = true
}

// Logged when a rotated login's save fails: the file was removed (unsavedRemovedMsg), or the
// filesystem refused even that (unsavedStuckMsg).
var (
	unsavedRemovedMsg = "the helper could not keep the machine login's refresh on disk; removed the machine file, which named a token the broker has rotated away: the login is held only in memory, the helper retries only the save every 15 s, and a restart before a save lands needs a new approval; " +
		cipherclient.Hint(cipherclient.FailureNoMachineLogin)
	unsavedStuckMsg = "the helper could not keep the machine login's refresh on disk, nor remove the machine file: a restart before the disk is writable again presents a token the broker has rotated away, and the broker revokes the machine as a reuse, ending its sessions; make the disk writable, and do not restart the helper until then"
)

// unsaveFile records cred, the held login whose save just failed with err, as unsaved, and
// removes the machine file at once (an unlink works on a full disk), so no restart presents the
// rotated-away token it names: a restart before a save lands holds no login. It logs
// unsavedRemovedMsg, or unsavedStuckMsg when even the removal fails (a read-only filesystem).
// The caller holds credMu.
func (b *Broker) unsaveFile(cred *machineCredential, err error) {
	b.unsaved = cred
	if rerr := RemoveMachineFile(b.MachineFile); rerr != nil {
		b.logger().Error(unsavedStuckMsg, "credential_id", cred.id, "machine_file", MachineFileNamed, "error", errors.Join(err, rerr))
		return
	}
	b.logger().Error(unsavedRemovedMsg, "credential_id", cred.id, "machine_file", MachineFileNamed, "error", err)
}

// retrySave saves cred, the unsaved login, and spends no rotation; a save that lands writes the
// file back and ends the unsaved record. The save's error is returned, for the loop to retry.
// The caller holds credMu.
func (b *Broker) retrySave(cred *machineCredential) error {
	err := b.saveFile(&MachineFile{Key: cred.key, CredentialID: cred.id, ExpiresAt: cred.expiresAt, RefreshToken: cred.refreshToken})
	if err == nil {
		b.unsaved = nil
		b.logger().Info("the helper kept the machine login's refresh on disk", "credential_id", cred.id, "machine_file", MachineFileNamed)
		return nil
	}
	b.logger().Warn("the helper still cannot keep the machine login's refresh on disk; retrying the save", "credential_id", cred.id, "machine_file", MachineFileNamed, "error", err)
	return err
}

// nextRefresh is how long the refresh loop waits before refreshing the held credential: half the
// time left until its expiry, at most every, so a credential the broker mints for less than two
// days is still refreshed well before it lapses.
func (b *Broker) nextRefresh(every time.Duration) time.Duration {
	cred := b.cred.Load()
	if cred == nil {
		return every
	}
	return min(every, max(0, time.Until(cred.expiresAt)/2))
}

// refreshLoop refreshes the held machine login after first, then at nextRefresh after each
// refresh that reached the broker and each install of a credential, and waits sched's answer after
// each refresh that failed (retrySchedule). A credential a launcher call's refusal dropped is
// settled at once (settleRefused), and retried the same way until it is. It ends with ctx.
func (b *Broker) refreshLoop(ctx context.Context, first time.Duration, sched retrySchedule) {
	every := b.refreshEveryOrDefault()
	sig := b.refreshSignals()
	// An install before the loop started asked for a reschedule first already accounts for.
	select {
	case <-sig.reschedule:
	default:
	}
	timer := time.NewTimer(first)
	defer timer.Stop()
	var settle *machineCredential
	for {
		select {
		case <-ctx.Done():
			return
		case <-sig.reschedule:
			if settle == nil {
				sched.reset()
				timer.Reset(b.nextRefresh(every))
			}
			continue
		case settle = <-sig.refused:
			timer.Reset(0)
			continue
		case <-timer.C:
		}
		var err error
		if settle != nil {
			err = b.settleRefused(ctx, settle)
			if err == nil || b.HasCredential() {
				// Settled, or settled in memory with its save failed: the held login is then
				// refreshed and saved like any other, at the retry below.
				settle = nil
			}
			// settleRefused's install asked for a reschedule the retry below replaces.
			select {
			case <-sig.reschedule:
			default:
			}
		} else {
			err = b.refresh(ctx)
		}
		timer.Reset(sched.after(err, func() time.Duration { return b.nextRefresh(every) }))
	}
}

// retrySchedule is the refresh loop's wait after a refresh that failed (next). The status alone
// decides whether the broker answered (oauthrefusal.Outage). A refresh the broker did not answer,
// or answered 5xx or 408, waits retry (15 s), and sets unknown: the broker may have rotated the
// token and lost its answer, and the token the helper still holds is redeemable again only inside
// the broker's five-minute rotation grace, whose start the helper cannot know. While unknown is
// set, every failure waits retry, an answered 4xx or 429 among them, until a refresh the broker
// settles (a success or invalid_grant, reset) clears it, so no run of answered refusals after a
// lost answer backs a retry past the grace. An outage longer than the grace right after a lost
// answer still ends in a reuse, which revokes the machine. Otherwise a 429, the outage that names
// its own wait, waits its Retry-After, at least retry and at most maxRetryAfter, and any other
// answer, a refusal with or without an OAuth error that ends no grant (redeem), waits backoff, from
// backoffStart doubling to maxRefreshRetry, until reset. An error that is no answer of the
// broker's (a token answer the helper cannot keep, a save that failed) waits retry too.
type retrySchedule struct {
	retry, backoffStart, backoff time.Duration
	unknown                      bool
}

func (s *retrySchedule) next(err error) time.Duration {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) || re.Response == nil || (oauthrefusal.Outage(re.Response.StatusCode) && re.Response.StatusCode != http.StatusTooManyRequests) {
		s.unknown = true
		return s.retry
	}
	if s.unknown {
		return s.retry
	}
	if re.Response.StatusCode == http.StatusTooManyRequests {
		return min(maxRetryAfter, max(s.retry, retryAfter(err)))
	}
	wait := s.backoff
	s.backoff = min(2*s.backoff, maxRefreshRetry)
	return wait
}

// reset restarts the backoff and clears unknown: a refresh the broker settled (a success, or
// invalid_grant), or a credential was installed.
func (s *retrySchedule) reset() {
	s.backoff, s.unknown = s.backoffStart, false
}

// errNoOutcome is refresh's answer when it found no credential to refresh (a launcher refusal
// dropped it meanwhile): nothing reached the broker, so it settles nothing.
var errNoOutcome = errors.New("no credential to refresh")

// after is the loop's wait after an attempt that returned err: nextRefresh's answer, with the
// schedule reset, after a refresh the broker settled (nil); unchanged state and retry for
// errNoOutcome, which neither resets the backoff nor clears unknown; next(err) otherwise.
func (s *retrySchedule) after(err error, nextRefresh func() time.Duration) time.Duration {
	switch {
	case err == nil:
		s.reset()
		return nextRefresh()
	case errors.Is(err, errNoOutcome):
		return s.retry
	}
	return s.next(err)
}

// redeem spends cred's refresh token for the rotated login: the machine file the broker's answer
// names, or refusal, the broker's invalid_grant code in a 400 or 401 answer
// (oauthrefusal.EndsGrant), when it has ended the login (revoked, lapsed or reused), or another
// error, logged, when the helper did not learn the outcome or cannot keep the answer. A 5xx
// naming invalid_grant is one of those, and so is the broker's 429 (its token endpoint's rate
// limit): the login is kept and retried after its Retry-After (retryAfter). A refusal that ends no
// grant - a 4xx with no OAuth error, another error, or invalid_grant at another status than 400 or
// 401 - is the broker answering, not down (oauthrefusal.Outage): the login is kept and the
// refresh backs off (retrySchedule). An answer naming no new refresh token is another: x/oauth2
// copies the presented token into an answer without a refresh_token field, and saving that would
// keep a token the broker may have rotated away, so the presented login is kept and the refresh
// retried. The caller holds credMu.
func (b *Broker) redeem(ctx context.Context, cred *machineCredential) (next *MachineFile, refusal string, err error) {
	cfg, err := b.oauthConfig()
	var tok *oauth2.Token
	if err == nil {
		tok, err = redact.OAuth2(cfg.TokenSource(b.tokenContext(ctx, cred.key),
			&oauth2.Token{RefreshToken: cred.refreshToken, Expiry: time.Unix(1, 0)}).Token())
	}
	if err != nil {
		err = b.URL.Error(err)
		var re *oauth2.RetrieveError
		switch {
		case errors.As(err, &re) && re.Response != nil && re.Response.StatusCode == http.StatusTooManyRequests:
			b.logger().Warn("the broker rate-limited the machine login's refresh; retrying after its Retry-After",
				"credential_id", cred.id, "retry_after", retryAfter(err), "error", err)
			return nil, "", err
		case oauthrefusal.EndsGrant(err):
			return nil, redact.InvalidGrant.String(), err
		}
		b.logger().Warn("the machine login's refresh did not reach the broker; retrying", "credential_id", cred.id, "error", err)
		return nil, "", err
	}
	if tok.RefreshToken == "" || tok.RefreshToken == cred.refreshToken {
		err := errors.New("the broker's token answer names no new refresh_token")
		b.logger().Error("the machine login's refresh answer cannot be kept; retrying", "credential_id", cred.id, "error", err)
		return nil, "", err
	}
	next, err = machineFileFrom(cred.key, tok)
	if err != nil {
		b.logger().Error("the machine login's refresh answer cannot be kept; retrying", "credential_id", cred.id, "error", err)
		return nil, "", err
	}
	return next, "", nil
}

// retryAfter is the wait a token endpoint answer names, read through serverwait.Read, or zero when
// err carries none or the answer names none. The broker's 429s name it in Retry-After alone. Its
// readers are the refresh's retrySchedule.next, which raises a zero to its retry and holds a wait
// to maxRetryAfter, a minute, so a wait of an hour waits the minute; redeem's line for a
// rate-limited refresh; and the device code poll (pollLogin), which waits it after an outage where
// it is longer than the interval, and never past the code's lifetime.
func retryAfter(err error) time.Duration {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) || re.Response == nil {
		return 0
	}
	wait, _ := serverwait.Read(re.Response.Header, time.Now())
	return wait
}

// removeRefusedFile deletes the machine file of a login the broker refused to refresh.
func (b *Broker) removeRefusedFile() {
	if err := RemoveMachineFile(b.MachineFile); err != nil {
		b.logger().Error("removing the refused machine login's file", "machine_file", MachineFileNamed, "error", err)
	}
}

// refresh spends the held login's refresh token for a new one and the credential's new expiry,
// under credMu, so it never races a login's install. It saves the new login to MachineFile, then
// swaps it in for the credential it refreshed. The broker's invalid_grant (the login was revoked,
// lapsed, or its refresh token was reused) drops the credential (dropRefreshRefused) and deletes
// the machine file; any other failure keeps both and is returned, so the loop retries. The old
// refresh token is spent once the broker answers, so a new login the file cannot take is still
// swapped in, and the failed save removes the machine file at once (unsaveFile) and is returned:
// until a save lands, every later refresh retries only the save (retrySave), never another token
// request. Holding no credential (a launcher refusal may have dropped it meanwhile), it does
// nothing and returns errNoOutcome, which the loop must not read as a settled refresh.
func (b *Broker) refresh(ctx context.Context) error {
	b.credMu.Lock()
	defer b.credMu.Unlock()
	cred := b.cred.Load()
	if cred == nil {
		return errNoOutcome
	}
	if b.unsaved == cred {
		return b.retrySave(cred)
	}
	f, refusal, err := b.redeem(ctx, cred)
	if refusal != "" {
		b.drop(cred, dropRefreshRefused, "code", refusal)
		b.removeRefusedFile()
		return nil
	}
	if err != nil {
		return err
	}
	saveErr := b.saveFile(f)
	next := f.credential()
	b.stateMu.Lock()
	swapped := b.cred.CompareAndSwap(cred, next)
	b.stateMu.Unlock()
	if swapped {
		close(cred.gone)
		go b.watchExpiry(next)
		b.logger().Info("machine login refreshed", "credential_id", f.CredentialID, "expires_at", f.ExpiresAt.Format(time.RFC3339))
	}
	if saveErr != nil {
		b.unsaveFile(next, saveErr)
	}
	return saveErr
}

// settleRefused refreshes cred, a credential the helper dropped because the broker answered a
// launcher call 401 LAUNCHER_INVALID, so its machine file follows the broker within seconds: a
// refresh the broker refuses (invalid_grant) deletes the file; one it answers keeps the rotated
// login in the file, since a 401 can also be a proof the broker could not verify (clock skew, an
// AGENT_SECRETS_URL mismatch), and a restart resumes it. The helper then still holds no
// credential, unless that save fails: the rotated login is then installed, so the helper keeps
// working and the refresh loop retries its save, and the save's error is returned. Once a newer
// login holds the file it does nothing; any other error is the refresh's whose outcome it did not
// learn, for the loop to retry. While cred is the unsaved login, it first retries only its save
// and makes no token request until that save lands.
func (b *Broker) settleRefused(ctx context.Context, cred *machineCredential) error {
	b.credMu.Lock()
	defer b.credMu.Unlock()
	if b.cred.Load() != nil {
		return nil
	}
	if b.unsaved == cred {
		if err := b.retrySave(cred); err != nil {
			return err
		}
	}
	f, refusal, err := b.redeem(ctx, cred)
	switch {
	case refusal != "":
		b.logger().Error(dropRefreshRefused+"; removed the machine file", "credential_id", cred.id, "code", refusal, "machine_file", MachineFileNamed)
		b.removeRefusedFile()
		return nil
	case err != nil:
		return err
	}
	if err := b.saveFile(f); err != nil {
		installed := f.credential()
		b.installCredential(installed, &loginState{State: "issued"})
		b.unsaveFile(installed, err)
		return err
	}
	return nil
}

// watchExpiry warns a day (expiryWarning) before cred lapses, at once when less than that is left,
// and drops cred when it reaches its expiry (dropExpired), as a broker refusal would: the login
// renews itself only while a refresh reaches the broker, and from then on no session can enroll
// under this machine's credential until a human approves a new machine login (a person signed in
// on the machine still enrols under their own sign-in). It ends early, silently, once the helper
// stops holding cred (a refusal, a refresh, or a newer login).
//
// In identity mode there is no warning at all, only the drop. The credential this machine's
// identity token trades for lives as long as that token, which Google bounds at an hour, so a
// lead of a day would fire on every trade; and what the warning tells an operator to do — run
// `agent-secrets machine login` and have a human approve it — is not how such a machine gets a
// credential at all. The trade loop owns that expiry, trading again well before it
// (cipherclient.Client.Run) and logging what it cannot trade.
func (b *Broker) watchExpiry(cred *machineCredential) {
	if b.testWatchEnded != nil {
		defer b.testWatchEnded(cred)
	}
	if b.Identity == nil && !b.warnBeforeExpiry(cred) {
		return
	}
	select {
	case <-b.after(cred, time.Until(cred.expiresAt)):
		b.dropOutsideRefresh(context.Background(), cred, dropExpired, "expires_at", cred.expiresAt.Format(time.RFC3339))
	case <-cred.gone:
	}
}

// warnBeforeExpiry waits until expiryWarning before cred lapses and says so, and reports whether
// the helper still holds cred then: false is a credential a refusal, a refresh or a newer login
// replaced while the watch waited, which the watch says nothing more about.
func (b *Broker) warnBeforeExpiry(cred *machineCredential) bool {
	lead := b.expiryWarning
	if lead == 0 {
		lead = 24 * time.Hour
	}
	select {
	case <-b.after(cred, time.Until(cred.expiresAt.Add(-lead))):
	case <-cred.gone:
		return false
	}
	if b.isGone(cred) {
		return false
	}
	b.logger().Warn(expiresSoonMsg,
		"credential_id", cred.id, "expires_at", cred.expiresAt.Format(time.RFC3339), "in", time.Until(cred.expiresAt).Round(time.Second))
	return true
}

// isGone reports whether the helper has stopped holding cred, so a timer that fired as it was
// replaced says nothing about it.
func (b *Broker) isGone(cred *machineCredential) bool {
	select {
	case <-cred.gone:
		return true
	default:
		return false
	}
}

// after is time.After(d), or testAfter's channel for cred when a test sets it.
func (b *Broker) after(cred *machineCredential, d time.Duration) <-chan time.Time {
	if b.testAfter != nil {
		if ch := b.testAfter(cred, d); ch != nil {
			return ch
		}
	}
	return time.After(d)
}

// dropOutsideRefresh is drop for a caller that does not hold credMu (watchExpiry, clearOnInvalid):
// it waits for a refresh in flight to finish, so a refresh that succeeds always ends with the
// helper holding the refreshed credential. A drop of the credential that refresh replaced then
// finds it gone and drops nothing. It reports whether it dropped cred and whether the helper holds
// a credential afterwards. A ctx that ends while it waits drops nothing either: the refresh it was
// waiting for settles cred its own way, and a later call the broker refuses under cred drops it
// then. watchExpiry's wait has no context to end.
func (b *Broker) dropOutsideRefresh(ctx context.Context, cred *machineCredential, why string, attrs ...any) (dropped, held bool) {
	if b.testBeforeDrop != nil {
		b.testBeforeDrop()
	}
	if b.credMu.LockContext(ctx) != nil {
		return false, b.HasCredential()
	}
	defer b.credMu.Unlock()
	return b.drop(cred, why, attrs...), b.HasCredential()
}

// expiresSoonMsg is the WARN watchExpiry logs a day before the held credential lapses.
// It is logged in machine-login mode alone: in identity mode watchExpiry warns of nothing.
var expiresSoonMsg = "the launcher credential lapses soon: no refresh has moved its expiry; the machine login renews itself while the helper reaches the broker, otherwise " +
	cipherclient.Hint(cipherclient.FailureNoMachineLogin)

// enrollBody is POST /v1/enrollments's shape in the shared broker contract (the v8 "approver"
// field is gone: the broker's policy picks a request's approver, never the enrollment).
// Operator is a pointer because the field is optional and "" is not an absent operator: the
// broker reads a stated empty operator as an operator that matches no credential and refuses
// the enrolment (enroll.Service.Create, ErrOperatorMismatch), so a credential that carries
// none sends no field at all.
type enrollBody struct {
	Kind       string  `json:"kind"`
	RuntimeID  string  `json:"runtime_id"`
	Operator   *string `json:"operator,omitempty"`
	Thumbprint string  `json:"thumbprint"`
	SessionID  *string `json:"session_id"`
	PodToken   *string `json:"pod_token"`
	// Projects are the Dispatch projects the session names; omitted for none, so a broker from
	// before the field reads the body as it always did.
	Projects []string `json:"projects,omitempty"`
}

// leaseReply is the answer of an enrolment and of a renew: the enrollment, and its lease. The
// enrollment id is the broker's text, or the text of whatever answers in its place, which can echo
// the launcher proof the enrolment carried. It is held as redact.RemoteText with that request's
// record (redact.Answer.Decode) and taken only as RemoteText.SendBack answers it (enrollUnder),
// since the helper keeps it, signs, renews and revokes with it, hands it to the socket's callers
// and logs it.
type leaseReply struct {
	EnrollmentID   redact.RemoteText `json:"enrollment_id"`
	LeaseExpiresAt time.Time         `json:"lease_expires_at"`
}

// launcherProof signs a launcher proof with the current machine credential, or fails with
// errNoCredential if there is none yet. It returns the credential it signed with, so a caller
// whose call the broker then refuses clears exactly that one (clearOnInvalid).
func (b *Broker) launcherProof(method, url string) (string, *machineCredential, error) {
	cred := b.cred.Load()
	if cred == nil {
		return "", nil, b.noCredential()
	}
	compact, err := proof.SignLauncher(cred.key, cred.id, method, url, time.Now())
	return compact, cred, err
}

// isLauncherInvalid reports whether err is the broker's 401 LAUNCHER_INVALID, its answer to any
// launcher proof it rejects: a credential that has expired or been revoked (a person's sign-in
// revoked with it), and also a proof it cannot verify, such as one signed outside its
// clock-skew window or for a URL other than its own.
func isLauncherInvalid(err error) bool {
	var be *BrokerError
	return errors.As(err, &be) && be.Status == http.StatusUnauthorized && be.HasCode("LAUNCHER_INVALID")
}

// clearOnInvalid drops the machine credential (dropRefused) and reports it missing whenever err
// is the broker's 401 LAUNCHER_INVALID. The broker answers that for any launcher proof it
// rejects: an expired or revoked credential, and also a proof it cannot verify, such as one
// signed outside its clock-skew window or for a URL other than its own (an AGENT_SECRETS_URL that
// does not match the broker's public URL). It never auto-relogins. cred is the credential the
// refused call was signed with, and only that one is dropped, after any refresh in flight
// (dropOutsideRefresh). A refusal of a credential a newer login or a refresh has already replaced
// drops nothing and is returned as it came: the helper holds a credential, and the caller's retry
// uses it. A ctx that ends while a refresh in flight holds credMu drops nothing as well, leaving
// that refresh to settle cred, which it does only when it succeeds or the broker answers
// invalid_grant: a refresh that fails with a 5xx, a timeout or a 429 settles nothing, and cred
// stays held until the refresh loop's next refresh settles it or a later call the broker refuses
// under it drops it, whichever comes first. The refusal is returned as it came while the helper
// still holds a credential: a broker-call op then ends at its bound with the broker's refusal, a
// known failure (ENROLL_FAILED, UNENROLL_FAILED), rather than waiting the refresh out until
// Server.brokerCall's timer answers its outcome unknown. A credential it drops is handed to the
// refresh loop, which refreshes it at once so its machine file is deleted if the broker has
// ended the login (settleRefused). In identity mode there is no refresh loop and no file: the
// trade loop is what holds the credential, so the drop wakes it instead
// (cipherclient.Client.Wake) and it trades again at once. Without that wake the helper would
// answer NO_CREDENTIAL to every register and sign until the loop's own renewal came round — ten
// minutes at a 900 s token, fifty-five at an hour's.
func (b *Broker) clearOnInvalid(ctx context.Context, cred *machineCredential, err error) error {
	if !isLauncherInvalid(err) {
		return err
	}
	dropped, held := b.dropOutsideRefresh(ctx, cred, dropRefused, "code", "LAUNCHER_INVALID")
	if dropped {
		if b.Identity != nil {
			b.Identity.Wake()
		} else {
			b.wakeRefused(cred)
		}
	}
	if !dropped && held {
		return err
	}
	return b.noCredential()
}

// drop stops holding cred, if the helper still does, records why, logs at ERROR that it did,
// naming why and the credential (with attrs), and reports whether it did: a refusal or an expiry
// that arrives after another login has installed a new credential leaves the new one alone and
// says nothing. The clear and the record of why are one write under stateMu, so no LoginStatus
// reads a credential gone without why: until a login is recorded, it reports the credential
// dropped, whatever the most recent login's state, and a login recorded meanwhile ends that
// record after it, never before. login-status — the probe every launcher decides on — then stops
// reporting a credential the helper no longer holds.
func (b *Broker) drop(cred *machineCredential, why string, attrs ...any) bool {
	b.stateMu.Lock()
	cleared := b.cred.CompareAndSwap(cred, nil)
	if cleared {
		if b.testClearHook != nil {
			b.testClearHook()
		}
		b.dropped = why
		if why == dropRefused {
			b.refusedCred = cred
		}
	}
	b.stateMu.Unlock()
	if !cleared {
		return false
	}
	close(cred.gone)
	b.logger().Error(why+"; cleared: "+b.dropLeaves(), append([]any{"credential_id", cred.id}, attrs...)...)
	return true
}

func (b *Broker) dropLeaves() string {
	return "no session can enroll under this machine's credential; " + cipherclient.Hint(b.noCredentialFailure())
}

// logger is b.Log, or a logger that writes nowhere when there is none.
func (b *Broker) logger() *slog.Logger {
	if b.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return b.Log
}

// EnrollIdentity enrols ONE reading of a session's identity, so the caller decides which
// generation the attempt belongs to rather than the broker rereading a session a rename may
// have moved on. The answer of an attempt that began under an identity the session no longer
// has must not be installed on the one it does, and must not be revoked either, since it may
// be the very enrollment the new identity is about to be handed back (dispatch://INFRA-1167).
//
// A named session states its omp session id as the enrolment's session_id. That is the name the
// broker's own carry is keyed on: an enrolment of the same (carry principal, session_id) adopts
// the enrollment the session had on the box it left, with the requests nobody has answered yet
// and the grants a person has given (enroll.Service.Create). Without it the broker sees an
// unnamed enrolment and the session follows nothing, which is the whole of what a machine
// session registered by the helper for itself lost. A session with no name states none.
func (b *Broker) EnrollIdentity(ctx context.Context, cur identity) (string, time.Time, error) {
	var sessionID *string
	if cur.SessionID != "" {
		sessionID = &cur.SessionID
	}
	return b.enroll(ctx, cur.RuntimeID, cur.Thumbprint, sessionID, cur.Projects, "host")
}

// EnrollSessionKey registers a key the helper does not hold, one a launcher enrolls for a session
// on this machine (a box's, a container's): a broker call with no registry Session, so it takes
// the key's runtime_id, thumbprint and optional session_id directly. Otherwise it is Enroll: the
// same fail-closed check when there is no machine credential yet, the same launcher proof, the
// same 200/201 handling and the same 401 LAUNCHER_INVALID handling.
func (b *Broker) EnrollSessionKey(ctx context.Context, runtimeID, thumbprint string, sessionID *string, projects []string) (string, time.Time, error) {
	return b.enroll(ctx, runtimeID, thumbprint, sessionID, projects, "box")
}

// enroll enrolls a key as kind session. A broker from before that kind (every release before
// the one that records one kind of machine session) answers it 400 INVALID_KIND, and enroll asks
// it once more with legacyKind, the kind that broker knew the enrollment by: host for the
// helper's own sessions, box for a key a launcher brings. That retry goes once the production
// broker records the session kind.
func (b *Broker) enroll(ctx context.Context, runtimeID, thumbprint string, sessionID *string, projects []string, legacyKind string) (string, time.Time, error) {
	id, lease, err := b.enrollAs(ctx, "session", runtimeID, thumbprint, sessionID, projects)
	var be *BrokerError
	if errors.As(err, &be) && be.Status == http.StatusBadRequest && be.HasCode("INVALID_KIND") {
		return b.enrollAs(ctx, legacyKind, runtimeID, thumbprint, sessionID, projects)
	}
	return id, lease, err
}

// signingAuthority is the launcher credential the helper signs a call about a session with, by
// the rule Broker's own comment gives: the person signed in on this box as this helper's user,
// where there is one, and this machine's own credential otherwise.
//
// It asks SignIns at every call rather than once at start, so a session registered after
// `agent-secrets login` acts for that person and one registered after the sign-in is revoked
// falls back to this machine, with no restart either way (cipherclient.SignInAuthority.Current
// says what that costs). A sign-in the broker has refused is no sign-in; every other failure to
// read one is returned, since a box that cannot tell whose sessions these are must not guess.
//
// machine is the held machine credential when that is what it chose, for the caller to drop if
// the broker refuses it (clearOnInvalid), and nil under a person's, which no drop of this
// machine's credential has anything to do with. It names no operator: only an enrolment states
// one (enrolAuthority).
func (b *Broker) signingAuthority(ctx context.Context) (*cipherclient.Authority, *machineCredential, error) {
	if b.SignIns != nil {
		authority, err := b.SignIns.Current(ctx)
		if err != nil {
			return nil, nil, err
		}
		if authority != nil {
			return authority, nil, nil
		}
	}
	cred := b.cred.Load()
	if cred == nil {
		return nil, nil, b.noCredential()
	}
	return &cipherclient.Authority{Key: cred.key, CredentialID: cred.id}, cred, nil
}

// enrolAuthority is signingAuthority with the operator the credential it chose carries, which
// is the only operator an enrolment may state: the person for a sign-in's credential; the
// OperatorFile's login for a machine login's, the login_hint that login was approved under; and
// none at all for the credential this machine's identity traded for, which names its fleet's
// service and no person. Stating an operator a credential does not carry is refused outright
// (enroll.Service.Create, ErrOperatorMismatch), which is why the machine-identity branch reads
// no operator file at all — and why a revoke, which states none, reads it on no branch.
func (b *Broker) enrolAuthority(ctx context.Context) (*cipherclient.Authority, *machineCredential, error) {
	authority, cred, err := b.signingAuthority(ctx)
	if err != nil || cred == nil || b.Identity != nil {
		return authority, cred, err
	}
	operator, err := b.readOperator()
	if err != nil {
		return nil, nil, err
	}
	authority.Operator = operator
	return authority, cred, nil
}

// enrollAs is one POST /v1/enrollments naming kind, signed with the credential enrolAuthority
// chose.
//
// A person's credential the broker rejects (401 LAUNCHER_INVALID: their sign-in was revoked, or
// the credential lapsed) is asked about once more rather than failing: the authority is told
// (SignInAuthority.Refused) and chosen again, which trades afresh and settles on this machine's
// own credential once the broker refuses the sign-in itself. So one enrolment is one answer,
// and the session a launcher is waiting on enrols under the authority the box has now.
func (b *Broker) enrollAs(ctx context.Context, kind, runtimeID, thumbprint string, sessionID *string, projects []string) (string, time.Time, error) {
	authority, cred, err := b.enrolAuthority(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	id, lease, err := b.enrollUnder(ctx, authority, cred, kind, runtimeID, thumbprint, sessionID, projects)
	if cred != nil || !isLauncherInvalid(err) {
		return id, lease, err
	}
	b.logger().Warn("the broker refused the sign-in's launcher credential; enrolling under whatever authority this box has now",
		"operator", authority.Operator, "credential_id", authority.CredentialID)
	if err := b.SignIns.Refused(ctx, authority.CredentialID); err != nil {
		return "", time.Time{}, fmt.Errorf("%s, to drop the credential the broker refused (%s): %w",
			cipherclient.SignInWaitEnded(authority.Operator, err), authority.CredentialID, err)
	}
	if authority, cred, err = b.enrolAuthority(ctx); err != nil {
		return "", time.Time{}, err
	}
	return b.enrollUnder(ctx, authority, cred, kind, runtimeID, thumbprint, sessionID, projects)
}

// enrollUnder is that one request, signed with authority. An operator is stated only where the
// credential carries one, since the broker refuses an enrolment that states an operator its
// credential does not have. cred is non-nil only where authority is this machine's own held
// credential, and a 401 LAUNCHER_INVALID drops exactly that one (clearOnInvalid); a person's
// credential is the caller's to settle. A 403 SESSION_HELD answers with the operator this
// request signed as, which is the principal the broker refused (sessionHeldUnder).
//
// Its failure is settled as known or unknown (requestReach.settle) before anything else reads
// it, and an answer with no enrollment_id is a 2xx the helper could not read, so it is unknown
// too; so is one whose enrollment_id the rule does not print as it came (RemoteText.SendBack),
// which may echo the proof this request carried and is kept, sent and printed nowhere. A refusal
// the caller retries (a 401 LAUNCHER_INVALID, a 400 INVALID_KIND) is a 4xx and so always known:
// an unknown outcome ends the op.
func (b *Broker) enrollUnder(ctx context.Context, authority *cipherclient.Authority, cred *machineCredential,
	kind, runtimeID, thumbprint string, sessionID *string, projects []string,
) (string, time.Time, error) {
	var operator *string
	if authority.Operator != "" {
		operator = &authority.Operator
	}
	body, err := json.Marshal(enrollBody{Kind: kind, RuntimeID: runtimeID, Operator: operator, Thumbprint: thumbprint, SessionID: sessionID, Projects: projects})
	if err != nil {
		return "", time.Time{}, err
	}
	ctx, reach := traceReach(ctx)
	req, htu, err := b.request(ctx, http.MethodPost, "/v1/enrollments", bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	compact, err := proof.SignLauncher(authority.Key, authority.CredentialID, http.MethodPost, htu, time.Now())
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Proof", compact)
	req.Header.Set("Content-Type", "application/json")
	var out leaseReply
	if err := b.do(req, []int{http.StatusOK, http.StatusCreated}, &out); err != nil {
		err = reach.settle(err)
		if cred != nil {
			err = b.clearOnInvalid(ctx, cred, err)
		}
		if sessionHeld(err) {
			// The broker refuses the name to the principal this request signed as and to no
			// other, and enrollLoop's ladder turns on which that was, so it travels with the
			// refusal rather than being chosen again after the answer (sessionHeldUnder).
			err = &sessionHeldUnder{operator: authority.Operator, err: err}
		}
		return "", time.Time{}, err
	}
	if out.EnrollmentID.IsEmpty() {
		return "", time.Time{}, reach.settle(fmt.Errorf("broker returned no enrollment_id"))
	}
	id, sent := out.EnrollmentID.SendBack()
	if !sent {
		return "", time.Time{}, reach.settle(errors.New("broker returned an enrollment_id the rule does not print as it came (not printed), which the helper keeps nowhere"))
	}
	return id, out.LeaseExpiresAt, nil
}

// Renew extends the lease with a proof signed by the session key. Unchanged: it always
// authenticates with the session's own key, never the launcher credential.
//
// It answers the identity reading it signed, so the caller can act on the result only while the
// session still holds that very enrollment: a refusal that arrives after a rename has reclaimed
// another identity speaks for the OLD enrollment, and applying it to the new one would lapse —
// and then revoke — an enrollment the broker is still honouring, with the grants it carries.
func (b *Broker) Renew(ctx context.Context, s *Session) (time.Time, identity, error) {
	// One reading: the enrollment id and the key that signs for it must be the same
	// generation's, or the proof names an enrollment the key does not hold.
	cur := s.identity()
	next, err := b.renewIdentity(ctx, cur)
	return next, cur, err
}

// renewIdentity renews the enrollment one identity reading names, with that reading's key. It
// answers the new lease alone: every caller already holds the reading it passed in, so handing
// that back would tell it nothing it does not have.
func (b *Broker) renewIdentity(ctx context.Context, cur identity) (time.Time, error) {
	id := cur.EnrollmentID
	req, htu, err := b.request(ctx, http.MethodPost, "/v1/enrollments/"+id+"/renew", nil)
	if err != nil {
		return time.Time{}, err
	}
	compact, err := proof.Sign(cur.Key, id, http.MethodPost, htu, time.Now())
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set("Proof", compact)
	var out leaseReply
	if err := b.do(req, []int{http.StatusOK}, &out); err != nil {
		return time.Time{}, err
	}
	return out.LeaseExpiresAt, nil
}

// Revoke deletes the enrollment and every grant under it. 204 and 404 both mean done.
//
// It signs with the credential enrolAuthority would choose now and, where the broker answers
// that this is not that enrolment's credential (403), with this machine's own. The broker lets
// a credential end only the enrolments of its own operator, and a machine's only those it made
// itself (enroll.authorized, ErrNotEnrollingCredential), while the helper keeps no note of
// which of the two enrolled a given session: a key it enrolled for a launcher leaves it none at
// all, and a restart leaves it the enrollment id alone. Two attempts settle every case, and
// only a 403 — the broker's "not this credential's" — costs the second.
func (b *Broker) Revoke(ctx context.Context, enrollmentID string) error {
	authority, cred, err := b.signingAuthority(ctx)
	if err != nil {
		return err
	}
	err = b.revokeUnder(ctx, authority, cred, enrollmentID)
	var be *BrokerError
	if cred != nil || !errors.As(err, &be) || be.Status != http.StatusForbidden {
		return err
	}
	machine := b.cred.Load()
	if machine == nil {
		return err
	}
	return b.revokeUnder(ctx, &cipherclient.Authority{Key: machine.key, CredentialID: machine.id}, machine, enrollmentID)
}

// revokeUnder is one DELETE /v1/enrollments/{id} signed with authority; cred is this machine's
// held credential where that is what signed, which a 401 LAUNCHER_INVALID drops. Its failure is
// settled as known or unknown (requestReach.settle) before anything else reads it, so Revoke's
// retry after a 403, a known refusal, never follows an unknown outcome.
func (b *Broker) revokeUnder(ctx context.Context, authority *cipherclient.Authority, cred *machineCredential, enrollmentID string) error {
	ctx, reach := traceReach(ctx)
	req, htu, err := b.request(ctx, http.MethodDelete, "/v1/enrollments/"+enrollmentID, nil)
	if err != nil {
		return err
	}
	compact, err := proof.SignLauncher(authority.Key, authority.CredentialID, http.MethodDelete, htu, time.Now())
	if err != nil {
		return err
	}
	req.Header.Set("Proof", compact)
	if err := b.do(req, []int{http.StatusNoContent, http.StatusNotFound}, nil); err != nil {
		var refused *BrokerError
		if !errors.As(err, &refused) {
			// A failed exchange names its request URL, and this one ends in the id the caller
			// gave, which for `unenroll --enrollment` is typed and was never accepted by the
			// broker: a credential pasted in its place would print. The route names the call
			// without it, and no Error, Cause or Class of the failure names the URL again.
			err = b.URL.Named(err, http.MethodDelete+" "+b.URL.Route("/v1/enrollments/(the enrollment id, not printed)"))
		}
		err = reach.settle(err)
		if cred == nil {
			return err
		}
		return b.clearOnInvalid(ctx, cred, err)
	}
	return nil
}

// do sends req and decodes the broker's answer, which it reads through redact.ReadAnswer. A failed
// exchange - the request, or the read of its answer - goes through the address's Error
// (cipheraddr.Address.Error). A refusal is a BrokerError with the answer's status and code, which
// the helper acts on, and with the broker's own words (the answer's error) as the address lets
// them print: beside an address that is not clean, the broker, or whoever answers for that address,
// can repeat the credential back in any text of its answer, so the refusal prints as its status
// alone (BrokerError.withheld), as oauthBrokerError and cipherclient's token exchange print theirs.
// An accepted answer out cannot be decoded from is an error that names the member that did not
// decode and quotes no value of the answer (redact.Answer.Decode).
func (b *Broker) do(req *http.Request, ok []int, out any) error {
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return b.URL.Error(err)
	}
	defer resp.Body.Close()
	answer, err := redact.ReadAnswer(resp, 1<<20)
	if err != nil {
		return b.URL.Error(err)
	}
	if !slices.Contains(ok, answer.Status()) {
		var e struct {
			Code  redact.RemoteText `json:"code"`
			Error redact.RemoteText `json:"error"`
		}
		_ = answer.Decode(&e)
		return newBrokerError(answer.Status(), e.Code, e.Error, !b.URL.Clean())
	}
	if out != nil && !answer.Words().IsEmpty() {
		if err := answer.Decode(out); err != nil {
			return fmt.Errorf("the broker's answer is not the JSON the helper reads: %w", err)
		}
	}
	return nil
}

// requestReach records how far one broker request got, for a caller that tells the outcome of
// that request to a client (traceReach, settle). Each record only ever moves to true, so a
// transport that sends the request twice cannot unsay one.
type requestReach struct {
	// sent is the whole request written (WroteRequest with no error), or the broker beginning to
	// answer (GotFirstResponseByte).
	sent atomic.Bool
	// connected is the transport holding a connection for the request (GotConn).
	connected atomic.Bool
}

// traceReach returns ctx with a trace that records, in the requestReach it returns, how far a
// request made under ctx got.
func traceReach(ctx context.Context) (context.Context, *requestReach) {
	r := &requestReach{}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { r.connected.Store(true) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				r.sent.Store(true)
			}
		},
		GotFirstResponseByte: func() { r.sent.Store(true) },
	}), r
}

// settle is err, the failure of the request r traced, as its caller is told it: marked as an
// unknown outcome (outcomeUnknown) when the request may have changed the broker's state, and as
// it came otherwise.
//
// A 4xx answer but a 408 is known: the broker decides its refusals before it writes anything, and
// a 429 is a rate limit, the broker's or a front door's, answered before the request runs. A 408
// is not known: the broker writes none, and a front door's runs out on its own clock, which can
// be after it forwarded the request (an Envoy proxy's stream_idle_timeout, Google's load
// balancer's client_timed_out), as mayHaveReachedServer in @legion/contracts/http-status says.
// The broker's own 503 MEMBER_KIND_UNCHECKABLE (uncheckableCode) is known too: it answers it when
// it cannot learn whether Dispatch calls the login's person a restricted member, before it writes
// anything, so nothing was enrolled, and the same request made later is asked afresh. Every other
// failure of a request that was sent is unknown: a transport error after the write (a reset, a
// timeout, the op's context ending at its bound), a 408, any other 5xx (a front door's 502 or 504
// can come after the broker wrote), and a 2xx the helper could not read (a body that will not
// read or decode, or one that lacks what the caller needs). A failure of a request that was never
// sent in full is known: a refused dial, a context that ended before the transport held a
// connection, a failed write. So is a failure before any request: signing a proof, reading the
// operator file, a sign-in's trade.
//
// The record is complete when the round trip returns, on every path but one. HTTP/1 waits for
// its write loop before it returns an error (persistConn.mapRoundTripError reads writeLoopDone),
// HTTP/2 waits for the stream on its other errors, and GotFirstResponseByte runs before either
// hands back a response. WroteRequest alone would not do for a response: either protocol can hand
// back a 2xx before it has finished the write of the request body, since each writes the request
// while it waits for the answer. The one path is HTTP/2's round trip whose context ends: it
// returns at once, and the write of the request can still be finishing.
// So a request whose context ended while the transport held a connection for it is unknown,
// whether or not the trace saw the write end.
func (r *requestReach) settle(err error) error {
	if err == nil {
		return nil
	}
	var be *BrokerError
	if errors.As(err, &be) && (be.Status >= 400 && be.Status < 500 && be.Status != http.StatusRequestTimeout ||
		be.Status == http.StatusServiceUnavailable && be.HasCode(uncheckableCode)) {
		return err
	}
	ended := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	if r.sent.Load() || (r.connected.Load() && ended) {
		return &outcomeUnknownError{err: err}
	}
	return err
}

// uncheckableCode is the code of the broker's own 503 for something it would do for a person
// while it cannot learn whether Dispatch calls that person a restricted member
// (projectaccess.UncheckableCode, spelled here as the helper reads it off the wire): it answers it
// before it writes anything, so the outcome is known (settle).
const uncheckableCode = "MEMBER_KIND_UNCHECKABLE"

// outcomeUnknownError marks the failure of a broker request whose outcome the helper does not
// know (requestReach.settle). It reads as the failure it wraps and unwraps to it, so every caller
// that tests a failure with errors.As or errors.Is, and every message made from one, reads it as
// before.
type outcomeUnknownError struct{ err error }

func (e *outcomeUnknownError) Error() string { return e.err.Error() }

func (e *outcomeUnknownError) Unwrap() error { return e.err }

// outcomeUnknown reports whether err is the failure of a broker request whose outcome the helper
// does not know: the broker may have acted on it.
func outcomeUnknown(err error) bool {
	var u *outcomeUnknownError
	return errors.As(err, &u)
}
