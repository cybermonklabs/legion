// packages/envoy/cmd/cipher/pod.go
//
// A Kubernetes pod's own enrollment. A pod an operator launches by
// hand holds no credential a person installed and has no agent-secrets-helper: what it has is the
// projected ServiceAccount token its own kubelet mounts, which the broker verifies against the
// cluster's OIDC issuer. So it logs in with that token at POST /v1/launcher-credentials/token for
// a launcher credential, and enrolls a session key of kind `pod` with that credential's launcher
// proof. Nothing is stored but the session key and its enrollment id, the two files file mode
// already reads (identity.go), so every later form — the exec form, request, self, renew — works
// from AGENT_SECRETS_KEY_DIR unchanged.
//
// The token must be the pod's own projected token, not one `kubectl create token` mints: the
// broker lets a pod's own machine login enrol that pod alone, and a login bound to no pod enrols
// nothing.
//
// This file never verifies the token — the broker does, against the cluster's issuer — and never
// prints it. It reads the token's claims only to name the pod.
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/redact"
)

// defaultPodTokenFile is where a Legion pod and a hand-launched session pod both mount their
// projected ServiceAccount token for the broker's audience, so a pod with the standard
// projection needs no --pod-token-file at all.
const defaultPodTokenFile = "/var/run/legion/agent-secrets-token/token"

// podIdentity is what a pod's projected token says about the pod, as the broker reads it too
// (enroll.AccountClaims).
type podIdentity struct {
	// Subject is the ServiceAccount the token was minted for,
	// system:serviceaccount:<namespace>:<name>; the broker names the launcher credential after it.
	Subject string
	// PodUID is the UID of the pod the token is bound to, the broker's runtime id for it.
	PodUID string
}

// runtimeID is the one string that names this pod at the broker: its enrollment's runtime_id. A
// pod UID is unique across the cluster and across time, so it names one pod by itself.
func (p podIdentity) runtimeID() string { return p.PodUID }

// String names the pod in a refusal, so an operator reading the pod's log knows which pod the
// broker refused and under which ServiceAccount.
func (p podIdentity) String() string {
	return fmt.Sprintf("account %s, pod uid %s", p.Subject, p.PodUID)
}

// podClaims reads the projected token's claims without verifying anything: the broker verifies the
// signature, the issuer, the audience and the expiry against the cluster's OIDC issuer, and this
// process has no key to check them with. It reads only what names the pod, and the broker refuses
// an enrollment whose runtime_id is not the pod UID its own verification of this same token found,
// so a claim read here can name this pod and nothing else.
//
// Reading the UID from the token rather than taking it as a flag is what spares the pod a
// downwardAPI entry for its own UID: the one thing the broker will accept is already in the file
// the pod must read anyway.
//
// A token it cannot read is refused in fixed text naming --pod-token-file, never in the decoder's
// words, which describe the token: the file holds a credential.
func podClaims(token string) (podIdentity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return podIdentity{}, errors.New("the projected service-account token in the file --pod-token-file names is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return podIdentity{}, errors.New("the claims of the projected token in the file --pod-token-file names are not base64url")
	}
	var claims struct {
		Subject    string `json:"sub"`
		Kubernetes struct {
			Pod struct {
				UID string `json:"uid"`
			} `json:"pod"`
		} `json:"kubernetes.io"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return podIdentity{}, errors.New("the claims of the projected token in the file --pod-token-file names are not JSON")
	}
	id := podIdentity{Subject: claims.Subject, PodUID: claims.Kubernetes.Pod.UID}
	if id.PodUID == "" {
		return podIdentity{}, errors.New("the projected token is bound to no pod (it names no kubernetes.io.pod.uid), so it enrolls nothing: a pod must present the token its own projection mounts, not one `kubectl create token` minted")
	}
	if id.Subject == "" {
		return podIdentity{}, errors.New("the projected token names no subject (sub), so it names no ServiceAccount to log in as")
	}
	return id, nil
}

// podLauncherSigner signs the pod's one enrollment call with the ephemeral launcher key the token
// login bound its credential to. The key never leaves this process's memory.
type podLauncherSigner struct {
	key          *ecdsa.PrivateKey
	credentialID string
}

func (s *podLauncherSigner) Sign(method, url string) (string, error) {
	return proof.SignLauncher(s.key, s.credentialID, method, url, time.Now())
}

// CreatePodEnrollment is POST /v1/enrollments of kind `pod`, authenticated by the launcher proof
// of the credential the token login just issued, and carrying the same projected token again so
// the broker checks the runtime id against the pod the token names. A key already holding this
// pod's live enrollment is answered 200 with that enrollment, so re-running enroll on a key dir
// that already has key.pem gets its own enrollment back; alreadyLive reports that answer, because
// the broker returns such an enrollment without extending its lease and the caller must renew it.
// projects are the Dispatch projects the pod names, sent only when it names one, as a session's
// enrollment sends them: the broker takes them from a person's pod, checked against its person,
// and refuses them from any other pod (PROJECTS_NEED_A_PERSON).
func (c *client) CreatePodEnrollment(ctx context.Context, signer proofSigner, runtimeID, thumbprint, podToken string, projects []string) (enrollmentID string, alreadyLive bool, err error) {
	body, err := json.Marshal(struct {
		Kind       string   `json:"kind"`
		RuntimeID  string   `json:"runtime_id"`
		Thumbprint string   `json:"thumbprint"`
		PodToken   string   `json:"pod_token"`
		Projects   []string `json:"projects,omitempty"`
	}{Kind: "pod", RuntimeID: runtimeID, Thumbprint: thumbprint, PodToken: podToken, Projects: projects})
	if err != nil {
		return "", false, err
	}
	status, raw, err := c.doProofStatus(ctx, signer, http.MethodPost, "/v1/enrollments", body)
	if err != nil {
		return "", false, err
	}
	var result struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	if err := c.decode(raw, &result, "decode enrollment response"); err != nil {
		return "", false, err
	}
	if result.EnrollmentID == "" {
		return "", false, errors.New("the broker's enrollment answered no enrollment_id")
	}
	return result.EnrollmentID, status == http.StatusOK, nil
}

// cmdEnrollPod implements "enroll --pod --service <name> [--pod-token-file <file>] [--out <dir>]
// [--projects <keys>]": the whole of a pod's enrollment, from the projected token to the
// enrollment file the exec form reads next, naming projects (--projects, or
// AGENT_SECRETS_PROJECTS) as a person's session does. It is idempotent over one directory: it
// keeps key.pem (a new key while the old enrollment is live would be refused ALREADY_ENROLLED),
// and when the broker answers that this key's enrollment is already live — which it returns
// without extending the lease — it renews that lease before it answers. Only the enrollment id
// reaches stdout; every other line, the next command included, is stderr's.
func cmdEnrollPod(service, tokenFile, out string, projects podProjects, stdout, stderr io.Writer) int {
	const prefix = "agent-secrets enroll"
	base := brokerBaseURL()
	for _, missing := range []struct {
		absent bool
		say    string
	}{
		{service == "", "--service is required: it names the broker-side service this pod's ServiceAccount is bound to"},
		{tokenFile == "", "--pod-token-file is required: it names the pod's projected service-account token file"},
		{out == "", "--out (or AGENT_SECRETS_KEY_DIR) is required"},
		{base.Empty(), brokerURLVar + " is required: it names the secrets broker this pod enrolls at"},
	} {
		if missing.absent {
			fmt.Fprintf(stderr, "%s --pod: %s\n", prefix, missing.say)
			return exitUsageError
		}
	}
	// The directory holds key.pem, so it is named by its flag and never by its path, and its
	// errors print the system's reason alone (redact.File), as key.pem's own do (outKeyFile).
	if err := os.MkdirAll(out, 0o700); err != nil {
		fmt.Fprintf(stderr, "%s --pod: make %s: %v\n", prefix, outDir, redact.File(err))
		return 1
	}
	unlock, err := lockEnrollDir(filepath.Join(out, enrollLockFile))
	if err != nil {
		if errors.Is(err, errNoEnrollLock) {
			fmt.Fprintf(stderr, "%s --pod: %v\n", prefix, err)
			return exitUsageError
		}
		fmt.Fprintf(stderr, "%s --pod: %v\n", prefix, err)
		return 1
	}
	defer unlock()
	return enrolPod(clock.Real(), service, tokenFile, out, projects, base, prefix, stdout, stderr)
}

// enrolPod is cmdEnrollPod's work, with the directory made and the lock held.
//
// Every refusal that leaves a key behind is written into the key dir's enrollment.error as well
// as printed. key.pem with no enrollment is the one state a later `agent-secrets` call cannot
// read on its own: selectSigner takes the key dir's branch on the key alone, finds no id, and
// without that file answers `read <dir>/enrollment: no such file or directory` — the broker's
// refusal lost, and the sign-in, helper and machine-signer branches below never reached. The
// key is kept on purpose (a new key while the old enrollment is live is refused
// ALREADY_ENROLLED), so the marker is what makes keeping it safe. A box's launcher writes the
// same file for the same reason.
//
// The broker answers 503 with Retry-After, refusing nothing, when it could not check the pod's
// token because the key set of the token's issuer could not be read. The login, the enrolment and
// the renewal of an enrollment the key already holds each wait such a wait out on clk and ask
// again, within one budget for the whole run (podOutages); past it, the run exits 1 with a line
// naming the outage and how long it tried (podOutage), recorded beside the key as a refusal is.
// clk is clock.Real() but in a test.
//
// No line here names the key directory or the token file by its path but the renewal command
// printed on success: it is a command to paste, and both paths were just used (the token file
// read and its token accepted, the directory holding the key.pem the broker enrolled), so it
// names the paths the run used, as the GCP forms' retry commands name the credentials file that
// signed them in. Every other line names them by their flags (outDir, outKeyFile,
// --pod-token-file).
func enrolPod(clk podClock, service, tokenFile, out string, projects podProjects, base cipheraddr.Address, prefix string, stdout, stderr io.Writer) int {
	ctx := context.Background()
	outages := podOutages{clock: clk, started: clk.Now()}
	// refuse prints line and, where this directory holds a key, records line beside it. A run
	// that has not reached enrollSessionKey yet leaves no key of its own, but an earlier run's
	// is enough: the test is the file, not how far this run got. Each caller returns 1 itself,
	// so every exit code in this command stays a literal the reference generator can read.
	refuse := func(line string) {
		fmt.Fprintln(stderr, line)
		if !exists(filepath.Join(out, keyFile)) {
			return
		}
		if err := writeFileAtomic(filepath.Join(out, enrollmentErrorFile), []byte(line+"\n")); err != nil {
			fmt.Fprintf(stderr, "%s --pod: record that refusal in %s in %s: %v\n", prefix, enrollmentErrorFile, outDir, redact.File(err))
		}
	}
	token, err := readPodToken(tokenFile)
	if err != nil {
		refuse(fmt.Sprintf("%s --pod: %v", prefix, err))
		return 1
	}
	identity, err := podClaims(token)
	if err != nil {
		refuse(fmt.Sprintf("%s --pod: %v", prefix, err))
		return 1
	}
	launcherKey, err := proof.NewKey()
	if err != nil {
		refuse(fmt.Sprintf("%s --pod: generate the login key: %v", prefix, err))
		return 1
	}
	c := newClient(base)
	// Each try of the login signs its request object anew, on the wall clock the broker reads it
	// by: the broker takes one issued within its skew of now, which a wait can outlast. The
	// identifier names this pod in the signed request object; the broker names the credential it
	// issues from the token's own subject, never from this.
	var credentialID string
	var signErr error
	err = outages.call(func() error {
		request, err := record.Sign(launcherKey, base.Raw(), []record.AuthorizationDetail{
			{Type: "launcher_credential", Identifier: identity.PodUID, Service: service},
		}, "", "", nil, time.Now())
		if err != nil {
			signErr = err
			return err
		}
		credentialID, err = c.TokenLogin(ctx, token, request)
		return err
	})
	if signErr != nil {
		refuse(fmt.Sprintf("%s --pod: sign the login request: %v", prefix, signErr))
		return 1
	}
	if err != nil {
		refuse(podRefusal(prefix, "log in", identity, base, err))
		return 1
	}
	key, err := enrollSessionKey(out)
	if err != nil {
		refuse(fmt.Sprintf("%s --pod: %v", prefix, err))
		return 1
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		refuse(fmt.Sprintf("%s --pod: thumbprint: %v", prefix, err))
		return 1
	}
	launcher := &podLauncherSigner{key: launcherKey, credentialID: credentialID}
	var enrollmentID string
	var alreadyLive bool
	err = outages.call(func() (err error) {
		enrollmentID, alreadyLive, err = c.CreatePodEnrollment(ctx, launcher, identity.runtimeID(), thumbprint, token, projects.keys)
		return err
	})
	if err != nil {
		refuse(podRefusal(prefix, "enrol", identity, base, err) + projects.refusal(err))
		return 1
	}
	if alreadyLive {
		// The broker gave back the enrollment this key already holds without extending its
		// lease, so a pod that only ever re-runs enroll would let that lease lapse.
		session := &fileSigner{key: key, enrollmentID: enrollmentID}
		if err := outages.call(func() error {
			_, err := c.RenewEnrollment(ctx, session, enrollmentID, token)
			return err
		}); err != nil {
			refuse(podRefusal(prefix, "renew the lease of the enrollment it already holds", identity, base, err))
			return 1
		}
	}
	if err := writeFileAtomic(filepath.Join(out, enrollmentFile), []byte(enrollmentID+"\n")); err != nil {
		refuse(fmt.Sprintf("%s --pod: write %s in %s: %v", prefix, enrollmentFile, outDir, redact.File(err)))
		return 1
	}
	// The id is written, so an earlier run's refusal is spent: selectSigner reads the error file
	// only where it finds no id, and it now finds one. Removed after the id rather than before
	// it, so a failed write above never leaves the directory with neither.
	if err := os.Remove(filepath.Join(out, enrollmentErrorFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "%s --pod: remove the earlier refusal in %s in %s: %v\n", prefix, enrollmentErrorFile, outDir, redact.File(err))
	}
	// A pod keeps no credential helper to re-run this command, so it needs a renewer: say which
	// one, on stderr, where it cannot be mistaken for the enrollment id. The key directory is
	// part of the command because renew finds its identity through AGENT_SECRETS_KEY_DIR, or
	// else the $XDG_RUNTIME_DIR default (identity.go, keyDir), and never through --out: a pod
	// that enrolled into a directory named by --out alone would otherwise be handed a command
	// that exits 1, finding no broker identity for this process (or, on a machine the broker
	// accepts on its own identity, acting as that machine rather than as this pod). Both words
	// are shell-quoted (shellWord), so the line pastes as it reads whatever the directory and
	// the token file are named.
	fmt.Fprintf(stderr, "%s --pod: enrolled; keep this lease alive with: AGENT_SECRETS_KEY_DIR=%s agent-secrets renew --pod-token-file %s\n",
		prefix, shellWord(out), shellWord(tokenFile))
	// The enrollment is the broker's, so it prints as the broker's address lets its answer print
	// (cipheraddr.Address.Words); the file above keeps it as it came.
	fmt.Fprintln(stdout, idWords(base, enrollmentID))
	return 0
}

// podRefusal is the one stderr line a refused login or enrolment prints: what was refused, which
// pod the token names, the HTTP status, the broker's code and message - the status and Message
// alone where the answer's text is withheld (apiError.withheld) - and the broker it asked. It
// never prints the token. An outage the run waited out for the whole of its budget is no refusal,
// and prints as what it is (podOutage): what the broker could not do, its answer as a refusal's
// words print it, and how long the run tried.
func podRefusal(prefix, what string, identity podIdentity, base cipheraddr.Address, err error) string {
	var outage *podOutage
	if errors.As(err, &outage) {
		return fmt.Sprintf("%s --pod: this pod (%s) could not %s: %v; asked %s", prefix, identity, what, outage, brokerSource(base))
	}
	var refused *apiError
	if errors.As(err, &refused) {
		return fmt.Sprintf("%s --pod: this pod (%s) could not %s: %s; asked %s", prefix, identity, what, refusalWords(refused), brokerSource(base))
	}
	return fmt.Sprintf("%s --pod: this pod (%s) could not %s: %s", prefix, identity, what, refusalText(brokerSource(base), err))
}

// podOutageBudget is how long one enroll --pod waits out the broker's outages, its token login's,
// its enrolment's and its renewal's together: a wait a 503 asks for (apiError.RetryAfter) is begun
// only where it ends inside the budget from the run's start (podOutages.call), so a pod's start
// learns of an outage within two minutes, as every client of the broker's token routes does.
const podOutageBudget = 2 * time.Minute

// podClock is the part of clock.Clock enroll --pod's waits read: when the run began, and each
// wait. It is clock.Real() but in a test, which hands it a clocktest.Fake.
type podClock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// podOutages is one enroll --pod's waits of the broker's outages, each on clock and all of them
// inside podOutageBudget from started.
type podOutages struct {
	clock   podClock
	started time.Time
}

// call is attempt's answer once each outage it met is waited out. While attempt answers a 503
// that asks for a wait (apiError.RetryAfter), call waits that wait and calls attempt again, for as
// long as the wait ends inside the run's budget; a wait that would end at or past it is not begun,
// and the answer is the outage (podOutage), saying how long the run tried. Every other answer is
// attempt's own, a 503 that asks for no wait among them.
func (o podOutages) call(attempt func() error) error {
	end := o.started.Add(podOutageBudget)
	for {
		err := attempt()
		var refused *apiError
		if !errors.As(err, &refused) || refused.RetryAfter <= 0 {
			return err
		}
		now := o.clock.Now()
		if !now.Add(refused.RetryAfter).Before(end) {
			return &podOutage{answer: refused, tried: now.Sub(o.started)}
		}
		<-o.clock.After(refused.RetryAfter)
	}
}

// podOutage is enroll --pod's answer when the broker's outage outlasted the run's budget: answer,
// the broker's last 503, and how long the run had tried by then. It is never a refusal: the broker
// decided nothing about the pod, and a later run may well enrol it.
type podOutage struct {
	answer *apiError
	tried  time.Duration
}

// Error names what the broker could not do - check the pod's token, because its issuer's key set
// could not be read, where the answer's code says so, and serve the call otherwise - in words of
// this client's own, which print whatever the address, then the answer as refusalWords prints it,
// how long the run tried, and the wait that would have run past the budget.
func (e *podOutage) Error() string {
	cause := "the broker could not serve the call"
	if e.answer.Code == issuerUnavailable {
		cause = "the broker could not check the pod's token, because its issuer's key set could not be read"
	}
	return fmt.Sprintf("%s (%s); tried for %s, and the wait of %s the broker asked for runs past the %s enroll --pod waits out an outage for",
		cause, refusalWords(e.answer), e.tried.Round(time.Second), e.answer.RetryAfter, podOutageBudget)
}

func (e *podOutage) Unwrap() error { return e.answer }

// refusalWords is a refusal of the broker's as every line of a renewer or an enrolment prints it:
// the HTTP status, the broker's code and message - the status and Message alone where the answer's
// text is withheld (apiError.withheld) - with UNKNOWN for a refusal that names no code.
func refusalWords(refused *apiError) string {
	if refused.withheld {
		return fmt.Sprintf("HTTP %d: %s", refused.Status, refused.Message)
	}
	// The code is the broker's, so it prints on one line as Message does.
	code := redact.OneLine(refused.Code)
	if code == "" {
		code = "UNKNOWN"
	}
	return fmt.Sprintf("HTTP %d %s: %s", refused.Status, code, refused.Message)
}

// podProjects is the Dispatch projects a pod names and where enroll --pod read them: --projects
// when the command line gave it, AGENT_SECRETS_PROJECTS otherwise. The two travel together, so a
// refusal of the list names the setting it came from (refusal) and never the list itself.
type podProjects struct {
	keys []string
	// flag is true when --projects named keys, and false when AGENT_SECRETS_PROJECTS did or
	// nothing named any.
	flag bool
}

// refusal is what enroll --pod adds to its line when the broker refused the list
// PROJECTS_NEED_A_PERSON, as it refuses a list from every pod but a person's. The broker's words
// are its own, which every client reads, and cannot say where this command read the list, so the
// line names the setting and says what a service's pod does with it. It never names the list.
// Any other answer adds nothing.
func (p podProjects) refusal(err error) string {
	var refused *apiError
	if !errors.As(err, &refused) || refused.Code != "PROJECTS_NEED_A_PERSON" {
		return ""
	}
	if p.flag {
		return ". The list came from --projects: only a person's pod names projects, so a service's pod enrols without --projects"
	}
	return ". The list came from " + cipherclient.ProjectsVar + ": only a person's pod names projects, so unset it for a service's pod"
}

// TokenLogin is POST /v1/launcher-credentials/token, the public route a service logs in at with
// a token its own issuer signed: no proof, no person. It answers the launcher credential the
// signed request object's key now holds.
func (c *client) TokenLogin(ctx context.Context, token, request string) (string, error) {
	body, err := json.Marshal(map[string]string{"token": token, "request": request})
	if err != nil {
		return "", err
	}
	raw, err := c.do(ctx, http.MethodPost, c.route("/v1/launcher-credentials/token"), body, nil)
	if err != nil {
		return "", err
	}
	var result struct {
		CredentialID string `json:"credential_id"`
	}
	if err := c.decode(raw, &result, "decode token login response"); err != nil {
		return "", err
	}
	if result.CredentialID == "" {
		return "", errors.New("the broker's token login answered no credential_id")
	}
	return result.CredentialID, nil
}

// enrollSessionKey is the session key in dir: the one already there, so re-running enroll on a dir
// keeps the key whose enrollment the broker holds live, or a new one written exactly as keygen
// writes it (PKCS8 PEM, 0600, in a 0700 directory, renamed into place). dir is the directory
// --out names, and its errors name key.pem by that flag (outKeyFile), never by its path.
func enrollSessionKey(dir string) (*ecdsa.PrivateKey, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("make %s: %w", outDir, redact.File(err))
	}
	path := filepath.Join(dir, keyFile)
	if exists(path) {
		return loadKey(path, outKeyFile)
	}
	key, err := proof.NewKey()
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	pemBytes, err := encodeKeyPEM(key)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, pemBytes); err != nil {
		return nil, fmt.Errorf("write %s: %w", outKeyFile, redact.File(err))
	}
	return key, nil
}

// outKeyFile is how every message names the session key in the directory keygen's and enroll
// --pod's --out names: by the flag that names its directory, since the key's path is the path of
// a file that holds a credential. main.go's loadKey reads it under this name, keygen and
// enrollSessionKey write it under it, and outDir names the directory itself.
const outKeyFile = "key.pem in " + outDir

// outDir is the directory keygen's and enroll --pod's --out (or AGENT_SECRETS_KEY_DIR) names, as
// a message names it.
const outDir = "the directory --out (or AGENT_SECRETS_KEY_DIR) names"

// enrollLockFile is the lock a pod's self-enrollment is serialized on, in the key dir it writes.
// A pod can run `enroll --pod` more than once over one directory, so two runs can overlap;
// without the lock the second would log in again (moving the launcher credential the first is
// still enrolling under) and the two would race over key.pem and enrollment.
//
// It is keyed on the directory, so two runs in one pod that name different --out directories
// take different locks and are not serialized: each logs in, and the second moves the credential
// the first enrolled under. That case is knowingly uncovered. A pod has no writable path of its
// own outside the key dir to key a lock on, and giving one pod two key directories is an
// operator's choice with no use this mode serves — one pod is one session. What ends the hazard
// rather than narrowing it is the broker's per-pod launcher credential, after which a second
// login of the same pod moves nothing another pod holds.
const enrollLockFile = "enroll.lock"

// errNoEnrollLock is lockEnrollDir's answer on a platform with no flock, where no Kubernetes pod
// of this cluster runs either: asking for --pod there is a usage error, not a failure of the
// machine.
var errNoEnrollLock = errors.New("enroll --pod runs only on a Linux pod: this platform has no flock to serialize concurrent enrollments on")
