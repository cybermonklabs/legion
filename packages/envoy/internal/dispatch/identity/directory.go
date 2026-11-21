package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	directory "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
	"google.golang.org/api/option/internaloption"
	htransport "google.golang.org/api/transport/http"
)

// DirectoryOutcome is what one Workspace Directory check found for a person, under IAP identity:
// Active confirms them, Held records a hold, Failed changes nothing, and Recreated is an address
// the Workspace deleted and created again, which the caller decides (Membership.checkDirectory).
type DirectoryOutcome int

const (
	// DirectoryFailed is every answer that is none of the others, and the zero value.
	DirectoryFailed DirectoryOutcome = iota
	// DirectoryActive is a 200 with suspended false for the person's own account.
	DirectoryActive
	// DirectoryHeld is a 200 with suspended true, a 404, or a 200 with suspended false for an
	// account that is not the person's own and whose primaryEmail is another address: the asked
	// address made an alias of a colleague's account.
	DirectoryHeld
	// DirectoryRecreated is a 200 with suspended false for the asked address itself (its
	// primaryEmail, letter case ignored) under another account than the stored one: the address
	// deleted and created again for another person. It confirms nobody by itself.
	DirectoryRecreated
)

// String is the outcome's name, as `envoy-dispatch directory-check` prints it.
func (o DirectoryOutcome) String() string {
	switch o {
	case DirectoryActive:
		return "Active"
	case DirectoryHeld:
		return "Held"
	case DirectoryRecreated:
		return "Recreated"
	default:
		return "Failed"
	}
}

// DirectoryFlight is the one deadline a Directory check and the write of its outcome share: the
// IAM signing request, the OAuth exchange, the Directory read and the database write. A check whose
// write has not committed within it is Failed.
const DirectoryFlight = 15 * time.Second

// The hosts a check reaches. The impersonate package fixes the first two.
const (
	signerHost       = "iamcredentials.googleapis.com"
	tokenHost        = "oauth2.googleapis.com"
	directoryUserURL = "https://admin.googleapis.com/admin/directory/v1/users/"
	// directoryFields are the fields a check asks users.get for, and the only ones it reads.
	directoryFields = "id,suspended,primaryEmail"
	// maxGoogleBody bounds what a check reads of any answer, as the impersonate package bounds its
	// own reads.
	maxGoogleBody = 1 << 20
)

// DirectoryError is a Failed check's error: the class of the failure and the HTTP status of the
// answer it failed on, and never a byte of what Google wrote, since an answer's body is Google's
// words and a check's error is logged and printed. It wraps the context's error when the check's
// context ended, so errors.Is tells a deadline from Google's answer.
type DirectoryError struct {
	// Class is the step that failed, signJwt, the token exchange or the Directory read, and how.
	Class string
	// Status is the HTTP status of the answer the step failed on, 0 when no answer arrived.
	Status int
	cause  error
}

// Reason is Class, with the HTTP status when there is one.
func (e *DirectoryError) Reason() string {
	if e.Status == 0 {
		return e.Class
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Class, e.Status)
}

func (e *DirectoryError) Error() string { return "identity: Directory check failed: " + e.Reason() }

func (e *DirectoryError) Unwrap() error { return e.cause }

// DirectoryReader checks a person's Workspace account through the Directory API's users.get, as a
// Workspace user (Domain-wide Delegation) through a signer service account, with no key: the
// process's own Google identity (on GKE, the Pod's workload identity) calls IAM signJwt on the
// signer for a JWT naming the delegated user and the Directory's read-only user scope
// (directory.AdminDirectoryUserReadonlyScope, the one scope its Domain-wide Delegation grant
// gives), and trades it at Google's token endpoint for an access token. It holds the
// authenticated client those requests are signed with, and shares one minted access token among
// its checks until the token expires.
//
// A check's Directory read runs under the check's own context. Its token comes from the shared
// token, or from the one mint every check that finds no valid token waits on (sharedMint), each
// only within its own context. The mint runs within the deadline of the check that started it,
// so each signJwt and token exchange ends inside the 15 seconds of a check (DirectoryFlight), and
// no check's cancel ends it for the others. The impersonate package carries no context into its
// two requests, so each mint builds its token source on a client of that mint alone (flight),
// whose transport puts the mint's context on every request the package sends; no context of a
// finished mint is kept.
type DirectoryReader struct {
	signer, admin string
	// opts are the caller's options, given again to each mint's token source.
	opts []option.ClientOption
	// signing is the client signJwt is sent through, authenticated as the process.
	signing http.RoundTripper
	// reading is the client the Directory read goes through, with no credential of its own: the
	// read carries the minted token.
	reading *http.Client
	// mintTimeout bounds a shared mint started by a check whose context has no deadline:
	// DirectoryFlight.
	mintTimeout time.Duration

	mu    sync.Mutex
	token *oauth2.Token
	// minting is the mint in flight, nil when none is.
	minting *sharedMint
}

// NewDirectoryReader is a reader that signs through signer, the service account's email, for
// admin, the delegated Workspace user's email. Construction finds the process's Application Default
// Credentials (on GCE and GKE that asks the metadata server whether it is there) and sends nothing
// to Google's APIs. opts reach Google through every client the reader builds; a deployment passes
// none, and its Application Default Credentials call signJwt. Tests pass option.WithHTTPClient
// (directorytest), which then carries all three requests, unauthenticated.
func NewDirectoryReader(ctx context.Context, signer, admin string, opts ...option.ClientOption) (*DirectoryReader, error) {
	if signer == "" || admin == "" {
		return nil, errors.New("identity: a Directory reader needs the signer account and the delegated Workspace user")
	}
	// The client library's own log is off whatever GOOGLE_SDK_GO_LOGGING_LEVEL says: at debug it
	// prints each request and answer, and the token exchange's answer is an access token.
	quiet := option.WithLogger(slog.New(slog.DiscardHandler))
	opts = slices.Clip(append([]option.ClientOption{quiet}, opts...))
	// The signing client is the one impersonate.CredentialsTokenSource builds for itself from the
	// same defaults, on the new auth library, whose transport fetches the process's own token
	// under each request's context, so a stalled metadata server ends with the flight too.
	signing, _, err := htransport.NewClient(ctx, append([]option.ClientOption{
		internaloption.WithDefaultAudience("https://" + signerHost + "/"),
		internaloption.WithDefaultScopes("https://www.googleapis.com/auth/cloud-platform"),
		internaloption.EnableNewAuthLibrary(),
	}, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("identity: the Directory reader's signing client: %w", err)
	}
	reading, _, err := htransport.NewClient(ctx, append([]option.ClientOption{option.WithoutAuthentication()}, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("identity: the Directory reader's reading client: %w", err)
	}
	d := &DirectoryReader{signer: signer, admin: admin, opts: opts, signing: signing.Transport, reading: reading, mintTimeout: DirectoryFlight}
	if d.signing == nil {
		d.signing = http.DefaultTransport
	}
	// The token source is built here once, so a configuration the impersonate package refuses is
	// refused at boot, not at every check.
	if _, err := d.tokenSource(ctx, &flight{ctx: ctx, base: d.signing}, directory.AdminDirectoryUserReadonlyScope); err != nil {
		return nil, fmt.Errorf("identity: the Directory reader's token source: %w", err)
	}
	return d, nil
}

// Check reads email's Directory user and answers its outcome, and the answer's id when Google
// answered a user. knownID is the Directory user id stored at the person's first Active outcome,
// empty before one. The answer is the person's own account when its id is knownID, or, with no
// knownID, when its primaryEmail is email with letter case ignored (Google addresses ignore case):
// a renamed person, whose old address is now an alias of their own account, keeps their id and is
// Active, and an address made an alias of a colleague's account answers with another id and
// another primaryEmail and is Held. An answer for email itself (its primaryEmail, case ignored)
// under another id than knownID is the address deleted and created again for another person, and
// is Recreated: whether it confirms anyone is the caller's to decide. A Failed outcome comes with
// a *DirectoryError. Check returns when ctx ends, at each of its steps. A shared mint it started
// runs to ctx's deadline at most, and goes on after ctx is canceled for the checks still waiting
// on it.
func (d *DirectoryReader) Check(ctx context.Context, email, knownID string) (DirectoryOutcome, string, error) {
	token, err := d.accessToken(ctx)
	if err != nil {
		return DirectoryFailed, "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, directoryUserURL+url.PathEscape(email)+"?fields="+directoryFields, nil)
	if err != nil {
		return DirectoryFailed, "", &DirectoryError{Class: "the Directory read could not be built"}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := d.reading.Do(request)
	if err != nil {
		return DirectoryFailed, "", unanswered(ctx, "the Directory read")
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return DirectoryHeld, "", nil
	case http.StatusUnauthorized:
		// Google no longer takes the token: the next check mints another.
		d.forget(token)
		fallthrough
	default:
		return DirectoryFailed, "", &DirectoryError{Class: "the Directory read answered an error", Status: response.StatusCode}
	}
	var user struct {
		ID           string `json:"id"`
		Suspended    *bool  `json:"suspended"`
		PrimaryEmail string `json:"primaryEmail"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxGoogleBody)).Decode(&user); err != nil {
		if ctx.Err() != nil {
			return DirectoryFailed, "", unanswered(ctx, "the Directory read")
		}
		return DirectoryFailed, "", &DirectoryError{Class: "the Directory read's answer did not decode", Status: http.StatusOK}
	}
	switch {
	case user.ID == "":
		return DirectoryFailed, "", &DirectoryError{Class: "the Directory read's answer has no id", Status: http.StatusOK}
	case user.Suspended == nil:
		return DirectoryFailed, "", &DirectoryError{Class: "the Directory read's answer has no suspended", Status: http.StatusOK}
	case *user.Suspended:
		return DirectoryHeld, user.ID, nil
	}
	sameAddress := strings.EqualFold(user.PrimaryEmail, email)
	switch {
	case knownID == "" && sameAddress, user.ID == knownID:
		return DirectoryActive, user.ID, nil
	case sameAddress:
		return DirectoryRecreated, user.ID, nil
	}
	return DirectoryHeld, user.ID, nil
}

// ProbeScope asks Google for a token for scope with the reader's signer and delegated user, as a
// check asks for the Directory scope, and answers whether Google granted it, or else the OAuth error
// code the token endpoint refused it with. Any other end - signJwt failing, no answer, a refusal
// whose code is not a plain OAuth error code - is an error, a *DirectoryError. The token is
// discarded. `envoy-dispatch directory-check` asks it for a scope the grant must not give.
func (d *DirectoryReader) ProbeScope(ctx context.Context, scope string) (granted bool, refusal string, err error) {
	f := &flight{ctx: ctx, base: d.signing}
	_, err = d.mintOn(f, scope)
	if err == nil {
		return true, "", nil
	}
	if code := f.exchanged().oauthCode; code != "" && ctx.Err() == nil {
		return false, code, nil
	}
	return false, "", err
}

// accessToken is the shared token while it is valid. A caller that finds none waits on the one
// mint in flight, and starts it when none is, so concurrent checks send one signJwt and one token
// exchange. Each caller waits only within its own ctx: one whose ctx ends fails alone, on the step
// the mint had reached, and the mint goes on for the others until its own deadline. When the
// mint's deadline passed - the deadline of the check that started it - a caller that can outlive
// it (no deadline, or a later one) waits on a new mint, which runs within that caller's deadline;
// any other caller's deadline has passed too, and it fails on the step the mint had reached.
func (d *DirectoryReader) accessToken(ctx context.Context) (string, error) {
	for {
		d.mu.Lock()
		if d.token.Valid() {
			token := d.token.AccessToken
			d.mu.Unlock()
			return token, nil
		}
		mint := d.minting
		if mint == nil {
			mint = d.startMint(ctx)
		}
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", mint.flight.abandoned(ctx.Err())
		case <-mint.done:
		}
		switch {
		case mint.err == nil:
			return mint.token.AccessToken, nil
		case !mint.lapsed:
			return "", mint.err
		case ctx.Err() != nil:
			return "", mint.flight.abandoned(ctx.Err())
		case !outlives(ctx, mint.deadline):
			// The caller's deadline is the lapsed mint's or earlier, so it has passed, though the
			// caller's own timer may not have fired yet: a new mint could not answer within it.
			return "", mint.flight.abandoned(context.DeadlineExceeded)
		}
	}
}

// outlives reports whether ctx can still be live after deadline: it has no deadline, or a later one.
func outlives(ctx context.Context, deadline time.Time) bool {
	own, ok := ctx.Deadline()
	return !ok || own.After(deadline)
}

// sharedMint is one mint of the Directory scope's token, which every check that finds no valid
// token waits on. Its context keeps the values and the deadline of the check that started it, and
// none of that check's cancellation, so a check that hangs up fails no other check, and each of
// the mint's requests ends inside the 15 seconds of the check that started it. A check whose
// context has no deadline gives the mint mintTimeout.
type sharedMint struct {
	flight *flight
	done   chan struct{}
	// deadline is the mint's context's deadline.
	deadline time.Time
	// token, err and lapsed are set before done closes.
	token *oauth2.Token
	err   error
	// lapsed is a mint that ended because its own deadline passed, before Google answered it.
	lapsed bool
}

// startMint starts the shared mint, with d.mu held.
func (d *DirectoryReader) startMint(ctx context.Context) *sharedMint {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(d.mintTimeout)
	}
	mintCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	mint := &sharedMint{flight: &flight{ctx: mintCtx, base: d.signing}, done: make(chan struct{}), deadline: deadline}
	d.minting = mint
	go func() {
		defer cancel()
		token, err := d.mintOn(mint.flight, directory.AdminDirectoryUserReadonlyScope)
		mint.token, mint.err = token, err
		mint.lapsed = err != nil && errors.Is(mintCtx.Err(), context.DeadlineExceeded)
		d.mu.Lock()
		if err == nil {
			d.token = token
		}
		d.minting = nil
		d.mu.Unlock()
		close(mint.done)
	}()
	return mint
}

// forget drops the shared token when it is still token.
func (d *DirectoryReader) forget(token string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.token != nil && d.token.AccessToken == token {
		d.token = nil
	}
}

// mintOn asks Google for a token for scope through a token source of f, within f's context. A
// failure is classed by f's steps.
func (d *DirectoryReader) mintOn(f *flight, scope string) (*oauth2.Token, error) {
	source, err := d.tokenSource(f.ctx, f, scope)
	if err != nil {
		return nil, &DirectoryError{Class: "the token source refused its configuration"}
	}
	token, err := source.Token()
	if err != nil {
		return nil, f.failure()
	}
	if token.AccessToken == "" {
		return nil, &DirectoryError{Class: "the token exchange's answer held no access token", Status: f.exchanged().status}
	}
	return token, nil
}

// tokenSource is the impersonate package's token source for scope, signed for the delegated user,
// on f's client.
func (d *DirectoryReader) tokenSource(ctx context.Context, f *flight, scope string) (oauth2.TokenSource, error) {
	return impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
		TargetPrincipal: d.signer,
		Scopes:          []string{scope},
		Subject:         d.admin,
	}, append(d.opts, option.WithHTTPClient(&http.Client{Transport: f}))...)
}

// unanswered is the failure of a step that got no answer: the context's end when it ended, and a
// transport failure otherwise.
func unanswered(ctx context.Context, step string) *DirectoryError {
	if err := ctx.Err(); err != nil {
		return &DirectoryError{Class: step + " did not answer before the check's context ended", cause: err}
	}
	return &DirectoryError{Class: step + " did not answer"}
}

// flight is the transport of one mint: it sends each request the impersonate package builds
// under the flight's context, and keeps the status of the signJwt and token exchange answers, and
// the OAuth error code of a refused exchange, so a failed mint is classed without reading the
// package's error, which quotes Google's answer.
type flight struct {
	ctx  context.Context
	base http.RoundTripper

	mu               sync.Mutex
	signer, exchange step
}

// step is what one of the two requests got.
type step struct {
	sent, answered bool
	status         int
	// oauthCode is a refused exchange's OAuth error code when it is one (oauthErrorCode).
	oauthCode string
}

func (f *flight) RoundTrip(request *http.Request) (*http.Response, error) {
	record := func(change func(*step)) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch request.URL.Host {
		case signerHost:
			change(&f.signer)
		case tokenHost:
			change(&f.exchange)
		}
	}
	record(func(s *step) { s.sent = true })
	response, err := f.base.RoundTrip(request.WithContext(f.ctx))
	if err != nil {
		return nil, err
	}
	code := ""
	if request.URL.Host == tokenHost && (response.StatusCode < 200 || response.StatusCode > 299) {
		body, err := io.ReadAll(io.LimitReader(response.Body, maxGoogleBody))
		_ = response.Body.Close()
		if err != nil {
			return nil, err
		}
		code = oauthErrorCode(body)
		response.Body = io.NopCloser(bytes.NewReader(body))
	}
	record(func(s *step) { s.answered, s.status, s.oauthCode = true, response.StatusCode, code })
	return response, nil
}

// abandoned is the failure of a caller whose own context ended, by cause, while it waited on f's
// mint: the step the mint had reached, which had not answered within that caller's context.
func (f *flight) abandoned(cause error) *DirectoryError {
	f.mu.Lock()
	defer f.mu.Unlock()
	step := "signJwt"
	if f.exchange.sent {
		step = "the token exchange"
	}
	return &DirectoryError{Class: step + " did not answer before the check's context ended", cause: cause}
}

// exchanged is what the token exchange got.
func (f *flight) exchanged() step {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchange
}

// failure classes a mint the impersonate package failed, by the last step the flight sent.
func (f *flight) failure() *DirectoryError {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, last := "signJwt", f.signer
	if f.exchange.sent {
		name, last = "the token exchange", f.exchange
	}
	switch {
	case f.ctx.Err() != nil || !last.answered:
		return unanswered(f.ctx, name)
	case last.status < 200 || last.status > 299:
		return &DirectoryError{Class: name + " answered an error", Status: last.status}
	default:
		return &DirectoryError{Class: name + "'s answer did not decode", Status: last.status}
	}
}

// oauthErrorCode is the error field of an OAuth error answer (RFC 6749 section 5.2) when it is a
// plain code, lowercase letters and underscores as every code the RFC and Google define is, and
// empty otherwise, so nothing else Google wrote is ever printed.
func oauthErrorCode(body []byte) string {
	var answer struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &answer) != nil || answer.Error == "" || len(answer.Error) > 64 {
		return ""
	}
	for _, r := range answer.Error {
		if r != '_' && (r < 'a' || r > 'z') {
			return ""
		}
	}
	return answer.Error
}
