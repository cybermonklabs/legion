// packages/envoy/cmd/dev-proxy-stands/enroll.go
// The `enroll-pod` subcommand: enrol one pod session at a local broker exactly as the Legion
// daemon enrols a worker pod, so the airlock's local stack has a session to ask for secrets
// with.
//
// Two calls. A service login, POST /v1/launcher-credentials/token, carrying the registrar
// account's projected ServiceAccount token (the stack's dev-oidc-issuer writes one) and a
// request object signed by a key this process makes, which wins a launcher credential no person
// approves. Then POST /v1/enrollments of kind "pod", authenticated with a launcher proof signed
// by that same key, carrying the pod's own UID, its projected token and the thumbprint of the
// key the session will sign with.
//
// Why here rather than in a command of its own: both calls are three lines each over
// internal/cipher/record and internal/cipher/proof, which this module already holds, and this
// command is already the local stack's one Go helper - the stands, the front and the enrolment
// are one binary the stack builds once. packages/daemon makes the same two calls in its
// cipher client, but that is another Go module and this one cannot import it.
//
// It makes no key for the session: `agent-secrets keygen` does that, and this takes the
// thumbprint it printed, so the private key stays in the key directory agent-secrets reads and
// never passes through this process.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/urljoin"
	"github.com/sjawhar/envoy/redirects"
)

// enrollTimeout bounds each of the two calls: a local broker answers in milliseconds, and a
// stack whose broker is wedged must fail rather than hang the script that started it.
const enrollTimeout = 30 * time.Second

// enrollReport is the JSON the enrolment writes to standard output.
type enrollReport struct {
	// CredentialID is the launcher credential the service login won.
	CredentialID string `json:"credential_id"`
	// EnrollmentID is what AGENT_SECRETS_KEY_DIR/enrollment holds and every session proof names.
	EnrollmentID string `json:"enrollment_id"`
	// LeaseExpiresAt is when the enrolment lapses unless the session renews it.
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// runEnrollPod is `dev-airlock-stands enroll-pod`.
func runEnrollPod(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dev-airlock-stands enroll-pod", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "the broker's base `<url>`, whose scheme, host and path are also the request object's audience")
	service := fs.String("service", "legion-daemon", "the service `<name>` BROKER_SERVICE_LOGINS lists the registrar account for")
	tokenFile := fs.String("token-file", "", "`<path>` of the registrar account's projected ServiceAccount token")
	podTokenFile := fs.String("pod-token-file", "", "`<path>` of the pod's own projected token, for the broker's audience")
	podUID := fs.String("pod-uid", "", "the pod's `<uid>`, which its token must name")
	thumbprint := fs.String("thumbprint", "", "the `<thumbprint>` of the session's signing key, as agent-secrets keygen printed it")
	session := fs.String("session", "", "the Envoy `<session id>` the broker notifies about this session's requests")
	slot := fs.String("slot", "", "which of the pod's independent enrolments this is, for a pod that holds several")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsageError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "dev-airlock-stands enroll-pod: unexpected argument %q\n", fs.Arg(0))
		return exitUsageError
	}
	for _, required := range []struct{ flag, value string }{
		{"--url", *url}, {"--token-file", *tokenFile}, {"--pod-token-file", *podTokenFile},
		{"--pod-uid", *podUID}, {"--thumbprint", *thumbprint},
	} {
		if required.value == "" {
			fmt.Fprintf(stderr, "dev-airlock-stands enroll-pod: %s is required\n", required.flag)
			return exitUsageError
		}
	}
	// The one rule both halves of this client read an address by, so --url takes an address
	// typed with however many trailing slashes exactly as agent-secrets does.
	base := cipherclient.WithoutTrailingSlashes(*url)
	loginURL, err := brokerRoute(base, "/v1/launcher-credentials/token")
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands enroll-pod: %v\n", err)
		return 1
	}
	// The enrolment's launcher proof signs TargetURI of that same join (brokerTarget): the broker
	// refuses a proof whose htu is not its own address followed by the path the proof was sent to,
	// which holds none of the query or user information --url may carry.
	enrollURL, err := brokerRoute(base, "/v1/enrollments")
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands enroll-pod: %v\n", err)
		return 1
	}
	enrollTarget, err := brokerTarget(base, "/v1/enrollments")
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands enroll-pod: %v\n", err)
		return 1
	}
	registrarToken, err := readToken(*tokenFile)
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands enroll-pod: %v\n", err)
		return 1
	}
	podToken, err := readToken(*podTokenFile)
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands enroll-pod: %v\n", err)
		return 1
	}

	key, err := proof.NewKey()
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands enroll-pod: generate the launcher key: %v\n", err)
		return 1
	}
	// The request object names the broker's own URL as its audience (proof.Audience): the broker
	// verifies it for its public URL exactly, which holds none of the query, fragment or user
	// information --url may carry. brokerRoute has refused a --url that does not parse.
	audience, err := proof.Audience(cipheraddr.New(base))
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands enroll-pod: --url %q: %s\n", redact.NotPrinted, redact.ClassNotAURL)
		return 1
	}
	// The request object names one launcher_credential detail whose service is the one the
	// broker holds the registrar account for; it refuses a login that names another.
	request, err := record.Sign(key, audience, []record.AuthorizationDetail{
		{Type: record.KindLauncherCredential, Identifier: *service, Service: *service},
	}, "", "", nil, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands enroll-pod: sign the request object: %v\n", err)
		return 1
	}
	var login struct {
		CredentialID string `json:"credential_id"`
	}
	if err := call(ctx, http.MethodPost, loginURL, nil,
		map[string]string{"token": registrarToken, "request": request}, &login); err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands enroll-pod: the service login: %v\n", err)
		return 1
	}
	if login.CredentialID == "" {
		fmt.Fprintln(stderr, "dev-airlock-stands enroll-pod: the service login answered no credential_id")
		return 1
	}

	body := map[string]any{
		"kind": "pod", "runtime_id": *podUID, "thumbprint": *thumbprint, "pod_token": podToken,
	}
	if *slot != "" {
		body["slot"] = *slot
	}
	if *session != "" {
		body["session_id"] = *session
	}
	signed, err := proof.SignLauncher(key, login.CredentialID, http.MethodPost, enrollTarget, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands enroll-pod: sign the launcher proof: %v\n", err)
		return 1
	}
	var enrolled struct {
		EnrollmentID   string    `json:"enrollment_id"`
		LeaseExpiresAt time.Time `json:"lease_expires_at"`
	}
	if err := call(ctx, http.MethodPost, enrollURL, map[string]string{"Proof": signed}, body, &enrolled); err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands enroll-pod: the enrolment: %v\n", err)
		return 1
	}
	if enrolled.EnrollmentID == "" {
		fmt.Fprintln(stderr, "dev-airlock-stands enroll-pod: the enrolment answered no enrollment_id")
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(enrollReport{
		CredentialID: login.CredentialID, EnrollmentID: enrolled.EnrollmentID, LeaseExpiresAt: enrolled.LeaseExpiresAt,
	}); err != nil {
		fmt.Fprintf(stderr, "dev-airlock-stands enroll-pod: write the report: %v\n", err)
		return 1
	}
	return 0
}

// readToken is one projected token file, less the newline a writer leaves on it.
func readToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("the token file %s is empty", path)
	}
	return token, nil
}

// brokerRoute is the URL of route at the broker: route joined onto the path of base, --url less
// its trailing slashes, with base's query kept and its fragment dropped (urljoin.Join), where route
// appended to base's text would sit inside a query or a fragment and the broker would be asked
// another path. A base that does not parse or names no host is refused by --url and the class
// alone: url.Parse's error quotes it whole, user information and query included.
func brokerRoute(base, route string) (string, error) {
	joined, err := urljoin.Join(base, route)
	if err != nil {
		return "", fmt.Errorf("--url %q: %s", redact.NotPrinted, redact.ClassNotAURL)
	}
	return joined.String(), nil
}

// brokerTarget is the htu a proof of a call to route at base signs: proof.TargetURI of the URL
// brokerRoute joins, which holds none of the query, fragment or user information base may carry.
func brokerTarget(base, route string) (string, error) {
	joined, err := urljoin.Join(base, route)
	if err != nil {
		return "", fmt.Errorf("--url %q: %s", redact.NotPrinted, redact.ClassNotAURL)
	}
	return proof.TargetURI(joined), nil
}

// call makes one JSON call and reads its answer into out. A refusal is an error naming the call by
// its method and its URL as redact.URL prints it, since --url can carry user information or a
// query, and the status and the broker's own body as the rule reads it (redact.Answer.Words).
func call(ctx context.Context, method, url string, headers map[string]string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, enrollTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(raw))
	if err != nil {
		// url.Parse's error quotes the URL whole, and a URL brokerRoute joined can be one it
		// refuses (an IPv6 zone holding a byte that is not ASCII, which url.URL.String escapes).
		return fmt.Errorf("%s %s: %w", method, redact.URL(url), redact.Class(err))
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	// The request carries a Proof header, so a redirect answered for the broker is refused.
	answer, err := redirects.Refused(http.DefaultClient).Do(request)
	if err != nil {
		// headers carry a proof or a token, which net/http's error can echo.
		return redact.Cause(err, url)
	}
	defer func() { _ = answer.Body.Close() }()
	read, err := redact.ReadAnswer(answer, 1<<20)
	if err != nil {
		return err
	}
	if answer.StatusCode != http.StatusOK && answer.StatusCode != http.StatusCreated {
		return fmt.Errorf("%s %s answered %d: %s", method, redact.URL(url), answer.StatusCode, read.Words())
	}
	return read.Decode(out)
}
