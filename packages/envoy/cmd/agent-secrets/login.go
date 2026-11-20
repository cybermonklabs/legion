// packages/envoy/cmd/agent-secrets/login.go
//
// `agent-secrets login`: the one command a person runs to sign in on a machine.
//
// It makes a box key, runs Google's device authorization grant (RFC 8628) for whoever is at the
// keyboard — asking for `openid` and `email` and nothing else, so the grant they give reaches
// nothing of theirs but who they are — and trades their identity token at the broker with this
// machine's own as the actor token. The broker records the sign-in against the box key's
// thumbprint and answers an access token that names the person and the machine, with a launcher
// credential of its own, under which this person's session on this machine enrolls.
//
// Nothing of the person's travels beyond the broker: the key is made here and only its public
// half is ever sent, and the refresh token is kept beside it in that person's own record, mode
// 0600 in a 0700 directory (brokerclient.SignIns).
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/redact"
)

// signInTimeout bounds a whole sign-in, the person's walk to another device included. The device
// code the issuer mints lives about as long.
const signInTimeout = 15 * time.Minute

// cmdLogin implements "login".
func cmdLogin(args []string, stdout, stderr io.Writer) int {
	flagArgs, positional := splitArgs(args, nil)
	if err := newFlagSet("login", stderr).Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	if len(positional) > 0 {
		return refuseUnexpectedArgument(stderr, "login")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, signInTimeout)
	defer cancel()
	if err := login(ctx, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "agent-secrets login: %v\n", err)
		return 1
	}
	return 0
}

// login runs the whole ceremony and keeps what it leaves.
func login(ctx context.Context, stdout, stderr io.Writer) error {
	base := brokerBaseURL()
	if base.Empty() {
		return fmt.Errorf("%s is required: it names the secrets broker this sign-in is recorded at", brokerURLVar)
	}
	flow, err := brokerclient.SignInFlow(clientGetenv)
	if err != nil {
		return err
	}
	if flow == nil {
		return fmt.Errorf("this machine is told of no sign-in client: set %s and %s to the device-flow client a person signs in with",
			brokerclient.SignInClientIDVar, brokerclient.SignInClientSecretFileVar)
	}
	machine, skipped, err := machineSource(ctx, true)
	if err != nil {
		return err
	}
	if machine == nil {
		return fmt.Errorf("a person signs in on a machine, and this machine proves no identity of its own (%s); set %s (%s or %s) to name one",
			joinSkipped(skipped), brokerclient.IdentityVar, brokerclient.IdentityMetadata, brokerclient.IdentityPool)
	}
	boxKey, err := proof.NewKey()
	if err != nil {
		return fmt.Errorf("make this machine's box key: %w", err)
	}
	thumbprint, err := proof.Thumbprint(&boxKey.PublicKey)
	if err != nil {
		return err
	}

	authorization, err := flow.Start(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(stderr, signInPrompt(authorization.VerificationURI, authorization.UserCode))
	signedIn, err := flow.Wait(ctx, authorization)
	if err != nil {
		return err
	}

	record := brokerclient.SignInRecord{
		Person: signedIn.Person, Issuer: flow.Issuer, ClientID: flow.ClientID,
		RefreshToken: signedIn.RefreshToken, BoxKey: string(keyPEM(boxKey)), Thumbprint: thumbprint,
		SignedInAt: time.Now().UTC(),
	}
	client, err := brokerclient.New(brokerclient.Config{
		BrokerURL: base,
		Key:       boxKey,
		Subject:   &brokerclient.PersonIdentity{Flow: flow, RefreshToken: signedIn.RefreshToken, Person: signedIn.Person},
		Actor:     machine,
		Session:   brokerclient.SessionFor(record),
		HTTP:      brokerclient.HTTPClient(),
	})
	if err != nil {
		return err
	}
	token, err := client.Token(ctx)
	if err != nil {
		return err
	}
	if token.CredentialID == "" {
		return fmt.Errorf("the broker took the sign-in but named no launcher credential, so this machine can enrol no session for %s", signedIn.Person)
	}
	if record.EnrollmentID, err = enrollUnderCredential(ctx, base, boxKey, thumbprint, token.CredentialID, signedIn.Person); err != nil {
		return err
	}
	if err := brokerclient.SignIns(homeGetenv).Write(record); err != nil {
		return err
	}
	// The enrollment is the broker's, so it prints as the broker's address lets its answer print
	// (brokeraddr.Address.Words); the record keeps it as it came. The person is the one Google's
	// identity token names.
	fmt.Fprintf(stdout, "%s is signed in on this machine (enrollment %s)\n", record.Person, base.Words(record.EnrollmentID))
	return nil
}

// signInPrompt is the line that tells a person where to sign in and what to type there: the
// issuer's verification page and user code, both the issuer's text. The page prints as
// internal/redact's URL prints it - whole where it is a plain http(s) URL, redact.NotPrinted
// otherwise, so a page that carries a user, a password or a query does not print - and the code
// on one line (redact.OneLine), so a line break in it cannot start a line of its own.
func signInPrompt(page, code string) string {
	return "To sign in, open " + redact.URL(page) + " and type the code " + redact.OneLine(code)
}

// keyPEM is a key as the bytes a record keeps, failing nothing: a key this process just made
// always marshals.
func keyPEM(key *ecdsa.PrivateKey) []byte {
	pemBytes, err := encodeKeyPEM(key)
	if err != nil {
		// An ECDSA key made by crypto/ecdsa always marshals; a failure here is a broken build.
		panic("agent-secrets: a key this process made could not be written: " + err.Error())
	}
	return pemBytes
}

// launcherKeySigner signs a launcher call with a credential this machine holds the key of: the
// box key a sign-in made, and the credential the trade minted under it. The helper's own
// launcher proofs are signed the same way, by the helper, with the key it holds.
type launcherKeySigner struct {
	key          *ecdsa.PrivateKey
	credentialID string
}

func (l *launcherKeySigner) Sign(method, url string) (string, error) {
	return proof.SignLauncher(l.key, l.credentialID, method, url, time.Now())
}

// enrollUnderCredential registers a session on this machine under the launcher credential a
// trade minted, and answers its enrollment id. who names whose session it is, for the refusal
// alone: a person's address, or the machine itself.
//
// It states no operator. An enrollment's operator is its credential's — the person a sign-in
// recorded, or nobody at all for a machine, whose session is its fleet's service's — and a
// stated operator that names anyone else, or any operator at all under a service credential, is
// refused (enroll.Service.Create, ErrOperatorMismatch).
//
// For a person, the key enrolled is the box key itself: one key per person per machine, which
// the broker already knows as the sign-in's, signs that session's proofs too. For a machine, it
// is the key the process made to prove the trade.
func enrollUnderCredential(ctx context.Context, base brokeraddr.Address, key *ecdsa.PrivateKey, thumbprint, credentialID, who string) (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]any{
		"kind": "session", "runtime_id": hostname, "thumbprint": thumbprint,
	})
	if err != nil {
		return "", err
	}
	raw, err := newClient(base).doProof(ctx, &launcherKeySigner{key: key, credentialID: credentialID},
		"POST", "/v1/enrollments", body)
	if err != nil {
		return "", fmt.Errorf("register %s's session on this machine: %w", who, err)
	}
	var answer struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil || answer.EnrollmentID == "" {
		return "", fmt.Errorf("the broker registered %s's session but named no enrollment", who)
	}
	return answer.EnrollmentID, nil
}

// registerSignInSession registers this person's session on this machine for a record that holds
// no enrollment yet: it trades the kept sign-in for an access token, as every later renewal
// does, enrolls the box key under the launcher credential that trade mints, and keeps the
// enrollment in the record. A machine or a sign-in client the deployment no longer configures is
// refused by name, and so is a record made with another client than this machine now has, since
// nothing here could renew that person's identity token.
func registerSignInSession(ctx context.Context, record brokerclient.SignInRecord, boxKey *ecdsa.PrivateKey) (brokerclient.SignInRecord, error) {
	base := brokerBaseURL()
	if base.Empty() {
		return record, fmt.Errorf("%s is required: it names the secrets broker %s's session registers at", brokerURLVar, record.Person)
	}
	flow, err := brokerclient.SignInFlow(clientGetenv)
	if err != nil {
		return record, err
	}
	switch {
	case flow == nil:
		return record, brokerclient.WithHint(fmt.Errorf("this machine is told of no sign-in client, so %s's sign-in cannot be renewed: set %s and %s, then sign in again",
			record.Person, brokerclient.SignInClientIDVar, brokerclient.SignInClientSecretFileVar), brokerclient.FailureSignInRefused)
	case record.ClientID != "" && record.ClientID != flow.ClientID:
		return record, brokerclient.WithHint(fmt.Errorf("%s signed in with another client than this machine now has (%s), so their sign-in cannot be renewed",
			record.Person, record.ClientID), brokerclient.FailureSignInRefused)
	}
	machine, skipped, err := machineSource(ctx, true)
	if err != nil {
		return record, err
	}
	if machine == nil {
		return record, fmt.Errorf("a person's session acts on a machine, and this machine proves no identity of its own (%s); set %s to name one", joinSkipped(skipped), brokerclient.IdentityVar)
	}
	client, err := brokerclient.New(brokerclient.Config{
		BrokerURL: base,
		Key:       boxKey,
		Subject:   &brokerclient.PersonIdentity{Flow: flow, RefreshToken: record.RefreshToken, Person: record.Person},
		Actor:     machine,
		Session:   brokerclient.SessionFor(record),
		HTTP:      brokerclient.HTTPClient(),
	})
	if err != nil {
		return record, err
	}
	token, err := client.Token(ctx)
	if err != nil {
		// The broker's own words name the cure where one exists, and an ended sign-in carries
		// its own hint, so a failed trade adds none (brokerclient.Hint).
		return record, err
	}
	if token.CredentialID == "" {
		return record, fmt.Errorf("the broker took %s's sign-in but named no launcher credential, so no session can be registered under it", record.Person)
	}
	if record.EnrollmentID, err = enrollUnderCredential(ctx, base, boxKey, record.Thumbprint, token.CredentialID, record.Person); err != nil {
		return record, err
	}
	return record, brokerclient.SignIns(homeGetenv).Write(record)
}
