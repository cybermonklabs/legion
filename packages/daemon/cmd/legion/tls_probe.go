package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

const (
	tlsDefaultProbeTimeout  = 20 * time.Second
	tlsChromiumProbeTimeout = 90 * time.Second
	tlsDetailLimit          = 300
	// bq's refusal of a chain tries its request three times, sleeping 2 and 4 s between the
	// tries, and each call starts three Python interpreters (the SDK's launcher, bq.py and the
	// `gcloud config` subprocess bq runs): measured on 2026-11-18 at 5-13 s of wall time and
	// 4.5-7 s of CPU a call on 2 vCPU.
	tlsBigQueryProbeTimeout = 60 * time.Second
)

type tlsSite struct {
	name, url, root string
	control         bool
}

var (
	emSignTLSSite    = tlsSite{name: "emSign", url: "https://dv-g3b-emudhra.we-have-a-problem.com/", root: "/usr/share/legion/tls-roots/emsign-ecc-root-ca-g3.pem"}
	aaaTLSSite       = tlsSite{name: "AAA", url: "https://dv-g3b-aaa-emudhra.we-have-a-problem.com/", root: "/usr/share/legion/tls-roots/aaa-certificate-services.pem"}
	controlTLSSite   = tlsSite{name: "control", url: "https://example.com/", control: true}
	isolationTLSSite = tlsSite{name: "isolation", url: "https://letsencrypt.org/"}
	tlsSites         = []tlsSite{emSignTLSSite, aaaTLSSite, controlTLSSite}
)

type tlsStatus string

const (
	tlsCertificateRefused tlsStatus = "refused"
	tlsConnectionAccepted tlsStatus = "accepted"
	tlsNetworkUnavailable tlsStatus = "unavailable"
	tlsClientAbsent       tlsStatus = "absent" // an optional client whose program is not on PATH (tlsOptionalClient)
)

type tlsOutcome struct {
	status tlsStatus
	detail string
}

type tlsClient interface {
	name() string
	probe(context.Context, tlsSite) tlsOutcome
}

type tlsObservation struct {
	client string
	site   tlsSite
	tlsOutcome
}

// The root paths and fingerprints below are CA-published records from
// repository.emsign.com/certs/emSignECCRootCAG3.crt (86:A1:EC:BA:08:9C:4A:8D:3B:BE:27:34:C6:12:BA:34:1D:81:3E:04:3C:F9:E8:A8:62:CD:5C:57:A3:6B:BE:6B)
// and crt.sectigo.com/AAACertificateServices.crt
// (D7:A7:A0:FB:5D:7E:27:31:D7:71:E9:48:4E:BC:DE:F7:1D:5F:0C:3E:0A:29:48:78:2B:C8:3E:E0:EA:69:9E:F4). example.com cannot prove
// isolation because its observed SSL.com chain is cross-signed by AAA Certificate Services.
func compromisedTLSSites() []tlsSite {
	var sites []tlsSite
	for _, site := range tlsSites {
		if site.root != "" {
			sites = append(sites, site)
		}
	}
	return sites
}
func tlsPositiveControlsLine(sites []tlsSite) string {
	var line strings.Builder
	line.WriteString("probe-image: TLS positive controls: ")
	for index, site := range sites {
		if index > 0 {
			if index == len(sites)-1 {
				line.WriteString(" and ")
			} else {
				line.WriteString(", ")
			}
		}
		line.WriteString(site.name)
	}
	line.WriteString(" chains verified")
	return line.String()
}

func verifyTLSPositiveControls(sites []tlsSite, probe func(tlsSite) tlsOutcome) error {
	var failures []string
	for _, site := range sites {
		if outcome := probe(site); outcome.status != tlsConnectionAccepted {
			failures = append(failures, fmt.Sprintf("%s positive control at %s did not accept its pinned root: %s", site.name, site.url, outcome.detail))
		}
		negative := isolationTLSSite
		negative.root = site.root
		if outcome := probe(negative); outcome.status != tlsCertificateRefused {
			failures = append(failures, fmt.Sprintf("%s positive control isolation endpoint %s was %s: %s", site.name, negative.url, outcome.status, outcome.detail))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("worker TLS probe cannot verify test chains: %s", strings.Join(failures, "; "))
	}
	return nil
}

// tlsPositiveControlSpec is the positive controls' curl. It is built through tlsClientSpec.build
// so that it runs as every client does, with a HOME of its own in a state directory and only the
// tlsKeptVariables of env, and its -q comes first for the reason the curl client's does
// (workerTLSClients).
var tlsPositiveControlSpec = tlsClientSpec{
	name: "positive-control", program: "curl", timeout: tlsDefaultProbeTimeout,
	command: func(site tlsSite, _, _ string) ([]string, []string) {
		return []string{"-q", "--cacert", site.root, "--capath", "/nonexistent", "--fail", "--silent", "--show-error", site.url}, nil
	},
}

func probeTLSPositiveControls(ctx context.Context, env []string, workDir string) error {
	stateDir, remove, err := newTLSStateDir()
	if err != nil {
		return err
	}
	defer remove()
	client := tlsPositiveControlSpec.build("", env, workDir, stateDir)
	return verifyTLSPositiveControls(compromisedTLSSites(), func(site tlsSite) tlsOutcome {
		return client.probe(ctx, site)
	})
}

var newTLSPositiveControls = probeTLSPositiveControls

// probeTLSClients holds every TLS client the worker image carries to the two compromised chains and
// a reachable control. A chain is safe only when its client refuses it for a certificate error; a
// network failure is neither a refusal nor an acceptance. An optional client the image does not
// carry is absent at each site, which is recorded and fails nothing.
func probeTLSClients(ctx context.Context, clients []tlsClient) ([]tlsObservation, error) {
	var observations []tlsObservation
	var failures []string
	for _, client := range clients {
		for _, site := range tlsSites {
			outcome := client.probe(ctx, site)
			observation := tlsObservation{client: client.name(), site: site, tlsOutcome: outcome}
			observations = append(observations, observation)
			switch {
			case outcome.status == tlsClientAbsent:
				// Only a tlsOptionalClient is absent, and only where its program is not on PATH.
			case site.control && outcome.status != tlsConnectionAccepted:
				failures = append(failures, fmt.Sprintf("network unavailable: %s cannot reach %s: %s", client.name(), site.url, outcome.detail))
			case !site.control && outcome.status == tlsConnectionAccepted:
				failures = append(failures, fmt.Sprintf("%s accepts the %s chain: %s", client.name(), site.name, outcome.detail))
			case !site.control && outcome.status != tlsCertificateRefused:
				failures = append(failures, fmt.Sprintf("%s cannot verify refusal of the %s chain: %s", client.name(), site.name, outcome.detail))
			}
		}
	}
	if len(failures) > 0 {
		return observations, fmt.Errorf("worker TLS probe failed: %s", strings.Join(failures, "; "))
	}
	return observations, nil
}

type tlsCommandClient struct {
	clientName string
	env        []string
	workDir    string
	timeout    time.Duration
	// command gives the program the client runs at a site, its arguments, and the variables it
	// sets, each NAME=value, on top of the variables of env it keeps (commandLine).
	command         func(tlsSite) (program string, args, set []string)
	applicationSeen func(string) bool
	// readStdout classifies stdout with stderr, for a program that writes its errors to stdout
	// (bq). Every other client's stdout is discarded: it is where a fetched page lands.
	readStdout bool
}

func (c tlsCommandClient) name() string { return c.clientName }

func (c tlsCommandClient) probe(ctx context.Context, site tlsSite) tlsOutcome {
	output, err := c.run(ctx, site)
	if tlsProbeDeadlineExceeded(ctx, err) {
		return tlsOutcome{status: tlsNetworkUnavailable, detail: "probe deadline"}
	}
	message := tlsCommandMessage(err, output)
	detail := trimTLSDetail(message)
	switch {
	case err == nil || c.applicationSeen != nil && c.applicationSeen(message):
		return tlsOutcome{status: tlsConnectionAccepted, detail: detail}
	case tlsCertificateError(message):
		return tlsOutcome{status: tlsCertificateRefused, detail: detail}
	default:
		return tlsOutcome{status: tlsNetworkUnavailable, detail: detail}
	}
}

// commandLine is the program c runs at site, its arguments, and the environment it runs under:
// the tlsKeptVariables of env, then the variables the command sets and LEGION_TLS_URL naming the
// site.
func (c tlsCommandClient) commandLine(site tlsSite) (program string, args, env []string) {
	program, args, set := c.command(site)
	return program, args, tlsEnvironment(c.env, tlsKeptVariables, append(set, "LEGION_TLS_URL="+site.url))
}

// run returns the command's stderr, with its stdout interleaved when the client reads stdout.
func (c tlsCommandClient) run(ctx context.Context, site tlsSite) (string, error) {
	program, args, env := c.commandLine(site)
	commandCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, program, args...)
	cmd.Env = env
	cmd.Dir = c.workDir
	var output bytes.Buffer
	cmd.Stdout = io.Discard
	if c.readStdout {
		cmd.Stdout = &output
	}
	cmd.Stderr = &output
	err := cmd.Run()
	return output.String(), err
}

func tlsProbeDeadlineExceeded(ctx context.Context, err error) bool {
	return err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)
}

// tlsOptionalClient is a command client for a program the image need not carry (workerTLSClients
// says which and why). At each probe it looks its program up with exec.LookPath, which is how
// exec.CommandContext resolves it, on the process's PATH; where that finds nothing — no file, or a
// link whose target is gone, since LookPath stats through the link — the client is absent for
// the site and runs nothing. Where the program is found, it is held to the chains as any client is.
type tlsOptionalClient struct {
	tlsCommandClient
	program string
}

func (c tlsOptionalClient) probe(ctx context.Context, site tlsSite) tlsOutcome {
	if _, err := exec.LookPath(c.program); err != nil {
		return tlsOutcome{status: tlsClientAbsent, detail: c.program + " is not on PATH"}
	}
	return c.tlsCommandClient.probe(ctx, site)
}

// tlsChromiumClient reads Chromium's navigation result from stderr because its headless DOM dump
// exits successfully after a failed navigation.
type tlsChromiumClient struct {
	tlsCommandClient
}

func (c tlsChromiumClient) probe(ctx context.Context, site tlsSite) tlsOutcome {
	stderr, err := c.run(ctx, site)
	if tlsProbeDeadlineExceeded(ctx, err) {
		return tlsOutcome{status: tlsNetworkUnavailable, detail: "probe deadline"}
	}
	if failure := chromiumNavigationFailure(stderr); failure != "" {
		detail := trimTLSDetail(failure)
		if tlsCertificateError(failure) {
			return tlsOutcome{status: tlsCertificateRefused, detail: detail}
		}
		return tlsOutcome{status: tlsNetworkUnavailable, detail: detail}
	}
	detail := trimTLSDetail(tlsCommandMessage(err, stderr))
	if err == nil {
		return tlsOutcome{status: tlsConnectionAccepted, detail: detail}
	}
	return tlsOutcome{status: tlsNetworkUnavailable, detail: detail}
}

func chromiumNavigationFailure(stderr string) string {
	const marker = "Page load failed:"
	for _, line := range strings.Split(stderr, "\n") {
		if _, failure, found := strings.Cut(line, marker); found {
			return marker + strings.TrimSpace(failure)
		}
	}
	return ""
}

// tlsKeptVariables are the only variables of the probe's environment a client runs with
// (tlsCommandClient.commandLine); every other variable of the pod's is dropped. It is a list of
// what to keep because a list of what to remove cannot be complete: the list of credential
// variables the probe used to unset missed SYSTEM_WGETRC, GIT_CONFIG_SYSTEM,
// CLOUDSDK_AUTH_AUTHORIZATION_TOKEN_FILE and MISE_NETRC_FILE, each measured making its client send
// a credential to the server it reached (TestImageTLSClientsReadNoSystemConfigurationOfThePods).
// Any client may read a variable nobody has found yet; a list of what to keep names all a client gets.
//
// Every client keeps one list of two parts. The first is the variables the worker image declares,
// PATH among them: sandbox.ImageEnv, which TestImageEnvIsWhatTheWorkerImageSets holds to the final
// stage's ENV in worker.Dockerfile. So the probe tests the trust store the image itself sets:
// AWS_CA_BUNDLE points the AWS CLI at Debian's bundle, and gcloud's and az's CA settings are no
// variable at all but the SDK's installation properties and az's wrapper. The second is the pod's
// trust and verification settings, tlsTrustVariables, which says which client reads which. They
// are kept because the probe tests the trust the roles in the pod get: the probe pod runs under
// the operator's pod env (probeManifest, internal/runtime/sandbox/probe.go), which every role
// inherits too, so a pod env that sets NODE_TLS_REJECT_UNAUTHORIZED=0, or points SSL_CERT_FILE at
// a bundle holding the emSign root, makes a role's node, or its curl, accept that chain, and must
// fail the probe. A trust setting holds a path, a flag, a list of host patterns or GODEBUG's
// settings, and no credential, so keeping one carries no credential to a site, and a setting the
// list does not name costs the probe its coverage, never a credential.
//
// Left out is every variable that names a file a client reads headers, logins or keys from,
// or, as GIT_CONFIG_PARAMETERS and GIT_CONFIG_COUNT do, carries such settings itself, since each
// can carry a credential: WGETRC and SYSTEM_WGETRC, GIT_CONFIG_GLOBAL, GIT_CONFIG_SYSTEM,
// GIT_CONFIG_PARAMETERS and GIT_CONFIG_COUNT, CURL_HOME, KUBECONFIG, BIGQUERYRC, CLOUDSDK_CONFIG,
// AWS_CONFIG_FILE, NETRC, MISE_NETRC_FILE and UV_CONFIG_FILE; and so are git's client certificate
// and key, GIT_SSL_CERT and GIT_SSL_KEY. A trust setting the pod carries only through one of those,
// as a wgetrc's check_certificate or a kubeconfig's insecure-skip-tls-verify, is not tested.
//
// On top of the kept variables, each client runs with the probe's own values: a HOME of its own
// in the state directory, which replaces the image's (tlsClientSpec.build), the settings its
// command gives, and LEGION_TLS_URL naming the site. A client that reads its configuration from
// its user's passwd home where that HOME holds none also takes the flag that stops it reading
// any: curl, the one such client measured, takes -q as its first argument (workerTLSClients). So
// no credential variable of the pod's reaches a client, and no credential file one names or the
// pod's home holds.
//
// A proxy variable of the pod's reaches no client either. That changes no verdict: Chromium already
// fails every load behind a proxy, so the probe cannot pass behind one (chromiumTLSArgs says why).
var tlsKeptVariables = slices.Concat(sandbox.ImageEnv, tlsTrustVariables)

// tlsTrustVariables are the pod's trust and verification settings every client keeps
// (tlsKeptVariables), each with the clients that read it. Each measurement below was made on
// 2026-11-18 in an INFRA issue pod against the emSign site, where "accepted" means the variable
// alone made the client accept the compromised chain. Not here, because no probed client reads it:
// PYTHONHTTPSVERIFY, which at 0 changed nothing for the image's python3, and PIP_CERT, for a pip
// the image does not put on PATH.
var tlsTrustVariables = []string{
	// OpenSSL's store and Go's: with SSL_CERT_FILE naming Debian's bundle plus the emSign root,
	// curl, python3, gh, kubectl, redis-cli ("Error: Server closed the connection") and socat
	// accepted, and curl did with SSL_CERT_DIR naming a hashed directory holding that root. go
	// reads both as gh does (crypto/x509), uv in its system-certs mode, and mise
	// (rustls-native-certs); wget and git (GnuTLS) read neither, nor do node and bun by default.
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	// curl: accepted.
	"CURL_CA_BUNDLE",
	// node, bun, omp and CodeGraph's node: 0 made node and bun accept.
	"NODE_TLS_REJECT_UNAUTHORIZED",
	// node's and bun's store: an extra CA, or the system's. An extra CA of the emSign root made
	// neither accept, since both answered "certificate revoked"; they are kept because they choose
	// the store. NODE_OPTIONS stays out, although its --use-openssl-ca and --use-system-ca flags
	// choose the store too: it also takes --require and --import, which run a script of the pod's
	// inside the client, so it holds more than a trust setting may (tlsKeptVariables).
	"NODE_EXTRA_CA_CERTS", "NODE_USE_SYSTEM_CA",
	// git: GIT_SSL_CAINFO naming the bundle with the emSign root made it accept.
	"GIT_SSL_NO_VERIFY", "GIT_SSL_CAINFO", "GIT_SSL_CAPATH",
	// go: GOINSECURE=* made it fetch the emSign page. GODEBUG's x509 and tls settings reach every
	// Go client: gh, go and kubectl.
	"GOINSECURE", "GODEBUG",
	// uv: UV_INSECURE_HOST naming the emSign host made it accept; UV_SYSTEM_CERTS and UV_NATIVE_TLS
	// choose its store (uv 0.13.5 warns that UV_NATIVE_TLS is deprecated for UV_SYSTEM_CERTS).
	"UV_INSECURE_HOST", "UV_SYSTEM_CERTS", "UV_NATIVE_TLS",
	// the AWS CLI: accepted with the bundle. sandbox.ImageEnv holds it too, since the image sets it.
	"AWS_CA_BUNDLE",
	// az, whose wrapper sets REQUESTS_CA_BUNDLE to Debian's bundle only where it is unset.
	"REQUESTS_CA_BUNDLE", "AZURE_CLI_DISABLE_CONNECTION_VERIFICATION",
	// gcloud and bq: the first, naming Debian's bundle, made the stock SDK, which trusts the roots it
	// bundles, refuse the emSign chain.
	"CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE", "CLOUDSDK_AUTH_DISABLE_SSL_VALIDATION",
}

// tlsEnvironment is the entries of base whose name keep holds and set gives no value, followed by
// set, where a later entry of set replaces an earlier one of its name (so a client's own HOME
// replaces the one build gives every client). Every entry of set is NAME=value: an entry without
// "=" is a mistake of this file's, which would set a variable nobody named, so tlsEnvironment
// panics on it rather than run the client. The tests compute every client's environment at every
// site (TestImageTLSClientsCarryNoCredentialOfThePods), so such an entry fails them first.
func tlsEnvironment(base, keep, set []string) []string {
	names := make([]string, len(set))
	for index, pair := range set {
		name, _, ok := strings.Cut(pair, "=")
		if !ok {
			panic(fmt.Sprintf("tlsEnvironment: set entry %q is not NAME=value", pair))
		}
		names[index] = name
	}
	out := make([]string, 0, len(keep)+len(set))
	for _, pair := range base {
		if name := tlsVariableName(pair); slices.Contains(keep, name) && !slices.Contains(names, name) {
			out = append(out, pair)
		}
	}
	for index, pair := range set {
		if !slices.Contains(names[index+1:], names[index]) {
			out = append(out, pair)
		}
	}
	return out
}

// tlsVariableName is the name of an environment entry, the text before its first "=".
func tlsVariableName(pair string) string {
	name, _, _ := strings.Cut(pair, "=")
	return name
}

func tlsCommandMessage(err error, output string) string {
	var parts []string
	if err != nil {
		parts = append(parts, err.Error())
	}
	if text := strings.Join(strings.Fields(output), " "); text != "" {
		parts = append(parts, text)
	}
	if message := strings.Join(parts, ": "); message != "" {
		return message
	}
	return "the command returned no output"
}

func trimTLSDetail(detail string) string {
	if len(detail) > tlsDetailLimit {
		head := tlsDetailLimit / 3
		return detail[:head] + "…" + detail[len(detail)-(tlsDetailLimit-head-1):]
	}
	return detail
}

func tlsCertificateError(detail string) bool {
	lower := strings.ToLower(detail)
	for _, marker := range []string{
		"cert_revoked",
		"certificate revoked",
		"certificate verify failed",
		"invalid peer certificate",
		// curl before 8.18 says "SSL certificate problem: <reason>"; 8.18 says "SSL certificate
		// OpenSSL verify result: <reason> (<code>)" for the same refusal (exit 60).
		"ssl certificate problem",
		"ssl certificate openssl verify result",
		"x509:",
		"failed to verify certificate",
		"net::err_cert",
		// GNU wget says "ERROR: The certificate of '<host>' …" for every certificate refusal.
		"error: the certificate of",
		// git's https transport, through libcurl's GnuTLS build: "server verification failed:
		// certificate signer not trusted".
		"server verification failed",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

const tlsFetchProgram = `const response = await fetch(process.env.LEGION_TLS_URL); if (!response.ok) throw new Error("HTTP " + response.status);`

// tlsClientSpec is one client of the TLS probe, which build makes: a client of workerTLSClients,
// or the positive controls' curl (tlsPositiveControlSpec).
type tlsClientSpec struct {
	// name is the client's name in the probe's output.
	name string
	// program is what the client runs, looked up on PATH where it is no path.
	program string
	timeout time.Duration
	// optional makes the client a tlsOptionalClient, absent where program is not on PATH.
	optional bool
	// chromium makes the client a tlsChromiumClient, which reads Chromium's navigation result.
	chromium bool
	// readStdout and applicationSeen are the command client's.
	readStdout      bool
	applicationSeen func(string) bool
	// command gives one site's arguments and the variables the client sets, each NAME=value, with
	// the client's state under stateDir (newTLSStateDir); invocation is the image's Oh My Pi.
	command func(site tlsSite, stateDir, invocation string) (args, set []string)
}

// build makes the client spec names, under env and in workDir. Every client gets a HOME of its own,
// <stateDir>/<name>, before the variables its command sets, so a client's own HOME replaces it.
func (spec tlsClientSpec) build(invocation string, env []string, workDir, stateDir string) tlsClient {
	home := "HOME=" + filepath.Join(stateDir, spec.name)
	client := tlsCommandClient{
		clientName: spec.name, env: env, workDir: workDir, timeout: spec.timeout,
		applicationSeen: spec.applicationSeen, readStdout: spec.readStdout,
		command: func(site tlsSite) (string, []string, []string) {
			args, set := spec.command(site, stateDir, invocation)
			return spec.program, args, append([]string{home}, set...)
		},
	}
	switch {
	case spec.chromium:
		return tlsChromiumClient{tlsCommandClient: client}
	case spec.optional:
		return tlsOptionalClient{tlsCommandClient: client, program: spec.program}
	default:
		return client
	}
}

// tlsProbeSignIn is the dummy credential gcloud, bq and kubectl sign in with: gcloud and bq from
// the state directory's tlsAccessTokenFile and, for bq, its --oauth_access_token flag, and kubectl
// from its --token flag. The probe needs their TLS handshake, never an authorized call. Its name
// holds no "token", so the public leak check does not read the literal as a credential.
const (
	tlsProbeSignIn     = "legion-tls-probe"
	tlsAccessTokenFile = "access-token"
)

// newTLSStateDir makes the directory every client of the TLS probe keeps its home in
// (tlsClientSpec.build), and gcloud, bq, az, mise, go and kubectl their state (go also changes to
// it, -C), so none of it lands in the runtime user's home (the image build's final step requires
// that) and no client reads a file of that home, and writes into it the access-token file gcloud
// and bq sign in with. remove deletes the directory with everything the clients wrote there.
func newTLSStateDir() (dir string, remove func(), err error) {
	dir, err = os.MkdirTemp("", "legion-tls-probe-")
	if err != nil {
		return "", nil, fmt.Errorf("make the TLS probe's state directory: %w", err)
	}
	remove = func() { _ = os.RemoveAll(dir) }
	if err = os.WriteFile(filepath.Join(dir, tlsAccessTokenFile), []byte(tlsProbeSignIn), 0o600); err != nil {
		remove()
		return "", nil, fmt.Errorf("write the TLS probe's access token: %w", err)
	}
	return dir, remove, nil
}

// googleCloudTLSEnvironment is the environment gcloud and bq run under, client naming the home
// build gives each under stateDir, which holds its configuration directory. The SDK signs in with
// the dummy token of the state directory's access-token file, which it reads because no variable
// of the pod's it would take a credential from first reaches it (tlsKeptVariables), so it needs
// neither an account nor a metadata server, and core/check_gce_metadata=false and a set
// metrics/environment are the two settings that stop it asking a metadata server whether it runs
// on GCE: measured on 2026-11-18 with the metadata server blackholed, each of those checks cost
// about 18 s of waiting, and with both set a call took 2 s. An image build has no metadata server
// and a cluster pod has one, so the outcome must not depend on it.
func googleCloudTLSEnvironment(stateDir, client string) []string {
	return []string{
		"CLOUDSDK_CONFIG=" + filepath.Join(stateDir, client, "config"),
		"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE=" + filepath.Join(stateDir, tlsAccessTokenFile),
		"CLOUDSDK_CORE_CHECK_GCE_METADATA=false",
		"CLOUDSDK_METRICS_ENVIRONMENT=legion-tls-probe",
		"CLOUDSDK_CORE_DISABLE_PROMPTS=1",
	}
}

// workerTLSClients is the worker image's TLS client inventory. It is complete by trust path, not
// by program: for each pair of a TLS stack and the CA store it trusts that a program on a role's
// PATH uses, at least one client here is held to the chains on every image. A program that carries
// its own TLS stack or CA bundle is probed itself. Every other program that links a TLS stack is
// named below under the probed client that shares its pair. The probe and its fakes derive their
// client names and timeout bounds from this list, so neither can silently fall out of tests.
//
// The census that finds them takes the first program of each name on a role's PATH,
// /opt/legion/bin:/opt/legion/pi-envoy/bin:/opt/omp/bin:/opt/codegraph/bin:/usr/local/bin:/usr/bin:/bin,
// resolves it through its links, and reads the file it reaches, as uid 1000:
//
//   - `ldd <file>` names a linked TLS library (libssl, libgnutls, libnss3);
//   - `go version -m <file>` names a Go program, and
//     `grep -a -F 'crypto/tls.(*Conn).Handshake' <file>` says whether it links Go's crypto/tls;
//   - `grep -a -E 'rustls|BoringSSL|OpenSSL [0-9]' <file>` finds a statically linked stack;
//   - a script runs its `#!` interpreter, which the census reads in its place.
//
// It ran on 2026-11-18 in a builds Job on the image of c665b75abc16,
// ghcr.io/cybermonklabs/legion-worker@sha256:25d2edfc2a8c1bb7c3cb83689456f8e2892d91a7fcd544f9cf6719528395c5fe.
// Perl has no TLS module in the image (IO::Socket::SSL, Net::SSLeay and LWP::Protocol::https are
// absent), and the system python3 has no certifi, requests, urllib3 or httpx. So a Perl script on
// PATH opens no TLS connection itself, and a Python script opens one only through python3's ssl
// module: OpenSSL with OpenSSL's default store.
//
// The pairs, the clients probed for each, the programs named under them, and the trust settings of
// tlsTrustVariables the pair's clients read:
//
//   - OpenSSL (libssl.so.3) with OpenSSL's default store, /usr/lib/ssl/cert.pem, which is Debian's
//     bundle: curl, python3 and redis-cli on every image, and socat where it is on PATH. Named
//     under them: redis-benchmark; the libpq programs pg_wrapper runs (psql, pg_dump, pg_dumpall,
//     pg_restore, pg_basebackup, pg_isready, pg_receivewal, pg_recvlogical, pgbench, createdb,
//     createuser, dropdb, dropuser, clusterdb, reindexdb, vacuumdb and vacuumlo), which verify
//     against that store only where a role names sslrootcert=system (psql, below); and every
//     Python script on PATH. Its clients read SSL_CERT_FILE and SSL_CERT_DIR, and curl
//     CURL_CA_BUNDLE too.
//   - GnuTLS with Debian's bundle, /etc/ssl/certs/ca-certificates.crt: wget, and git, whose https
//     transport, git-remote-https, links libcurl's GnuTLS build (curl itself links OpenSSL). git
//     reads GIT_SSL_NO_VERIFY, GIT_SSL_CAINFO and GIT_SSL_CAPATH; wget reads no trust variable,
//     only a wgetrc's settings, and tlsKeptVariables leaves the wgetrc variables out.
//   - Go's crypto/tls with the system roots crypto/x509 reads: gh, go and kubectl. Named under
//     them: legion, agent-secrets and gopls. yq and gofmt are Go programs that do not link
//     crypto/tls. Each reads SSL_CERT_FILE, SSL_CERT_DIR and GODEBUG, and go GOINSECURE too.
//   - A stack or a CA bundle of the program's own, so each of these is probed itself: node, and
//     the CodeGraph runtime (/opt/codegraph/node, a link to the same Node), with Node's bundled
//     stack and roots, the pair a `#!/usr/bin/env node` script uses too, both reading
//     NODE_TLS_REJECT_UNAUTHORIZED, NODE_EXTRA_CA_CERTS and NODE_USE_SYSTEM_CA; bun, and omp, which
//     is a Bun program, with BoringSSL and Bun's roots, bun reading the same three and omp
//     NODE_TLS_REJECT_UNAUTHORIZED; uv and mise, which link rustls, uv reading
//     UV_INSECURE_HOST, UV_SYSTEM_CERTS and UV_NATIVE_TLS, and in its system-certs mode
//     SSL_CERT_FILE and SSL_CERT_DIR, which mise reads too; the AWS CLI, whose Python and OpenSSL
//     are its own and which verifies against Debian's bundle, not botocore's own
//     (awscli/botocore/cacert.pem in its dist tree), because the image sets
//     AWS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt, the setting it reads; gcloud and
//     bq, which bundle their own Python and are pointed at Debian's bundle, reading
//     CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE and CLOUDSDK_AUTH_DISABLE_SSL_VALIDATION; az, whose Python
//     is its own and whose wrapper points requests at Debian's bundle, reading REQUESTS_CA_BUNDLE
//     and AZURE_CLI_DISABLE_CONNECTION_VERIFICATION; and Chromium, with NSS and its own root store,
//     which reads none of them.
//
// The census reads a script as its `#!` interpreter, which hides the client a shell wrapper runs.
// Each of these runs a probed client and uses that client's pair: the gh and git wrappers in
// /opt/legion/bin (gh through `legion gh` to the real gh, git through `legion git` to
// /usr/bin/git); the jj wrapper there, whose jj runs git for its fetches and pushes; `dispatch` in
// /opt/legion/pi-envoy/bin, which runs bun; `codegraph` in /opt/codegraph/bin, which runs
// CodeGraph's node; `wcurl`, which runs curl; and `socat-broker.sh`, `socat-chain.sh` and
// `socat-mux.sh`, which run socat.
//
// wget, git, go, kubectl and redis-cli are floor clients, held to the chains on every image.
// wget, go, kubectl and redis-cli are on the toolchain row of the floor every deployment's image is
// held to (packages/daemon/internal/capabilities/image.go); git is on no row, but every issue pod
// runs `legion workspace-init provision` on the image in its workspace-init container, and that
// command needs git on PATH (provisioningTools). go and kubectl are probed although gh holds their
// pair, because each reaches a URL a role names: `go install <module>@<version>` and `go get` with
// GOPROXY=direct fetch `https://<host>/<path>?go-get=1` from the module path's host, and
// `kubectl apply -f https://…` and `kubectl --server` reach the host the role gives. redis-cli is
// probed although curl and python3 hold its pair for the same reason: `redis-cli --tls` reaches the
// host and port a role gives.
//
// gcloud, bq, az, mise and socat are probed where they are on PATH and reported absent otherwise
// (tlsOptionalClient). None is on the floor, so an image built on another base may not carry it.
// Such an image may also carry this image's /usr/local/bin links to gcloud, bq and az without the
// /opt trees those links point into, as LINUX's worker-lean, which copies /usr/local/bin onto
// Ubuntu, may; socat lives in /usr/bin, which worker-lean does not copy. Where one is on PATH, it
// must refuse both chains and reach the control, as every other client must. mise is here because
// its http: backend downloads from any URL a role names, with its own HTTP client verifying against
// the system roots, and socat because its OPENSSL: address verifies its peer against OpenSSL's
// default store.
//
// No client carries a credential of the pod's to a site: of the pod's variables, each keeps only
// those the image declares and every trust setting named beside the pairs above (tlsKeptVariables,
// which says why it is a list of what to keep and why a trust setting carries no credential); it
// runs with a HOME of its own in the state directory; and curl, which reads the curlrc of its
// user's passwd home where CURL_HOME, XDG_CONFIG_HOME and HOME hold none, runs with -q first,
// which stops it reading any curlrc. So neither a credential variable of the pod's nor a
// credential file of the pod's home reaches a client. The positive controls' curl is built the
// same way (tlsPositiveControlSpec).
//
// Deliberately not here, each with what was measured:
//
//   - busybox's wget and ssl_client applets, which do no certificate validation at all (busybox's
//     own help says "TLS certificate validation not implemented"), and which the image makes
//     reachable only through the busybox binary, as `busybox wget`: Debian's busybox package
//     installs no applet links, so the image's `wget` is GNU wget, the one the capability check's
//     toolchain row requires on PATH. An applet that became that `wget` would stop at --no-hsts,
//     an option it does not know, so the probe would fail it as unable to verify a refusal; it
//     never reaches the chains (measured on 2026-11-18).
//   - gsutil, which the image does not put on PATH: it stays inside /opt/google-cloud-sdk/bin.
//   - psql (PostgreSQL 18.6's client, on the toolchain row) and the other libpq programs above,
//     which cannot reach a certificate verdict at an HTTPS site: libpq's TLS starts after
//     Postgres's SSLRequest, which the sites answer as HTTP ("received invalid response to SSL
//     negotiation" at the emSign site and at example.com), and its direct mode
//     (sslnegotiation=direct) needs the postgresql ALPN protocol, which the emSign site refuses
//     before any certificate check ("SSL error: unexpected eof while reading") and example.com
//     does not negotiate ("direct SSL connection was established without ALPN protocol
//     negotiation extension"). Its default sslmode=prefer verifies no certificate, and its
//     verify-ca and verify-full modes read ~/.postgresql/root.crt unless a role names
//     sslrootcert=system, which is OpenSSL's default store, the store curl, python3, redis-cli and
//     socat are probed through. Measured with psql on 2026-11-18.
//   - openssl's CLI (OpenSSL 3.5.7, /usr/bin/openssl): `openssl s_client` completes the handshake
//     whatever its verification says and exits 0 (against the emSign site it printed "Verify
//     return code: 20 (unable to get local issuer certificate)" and exited 0), so it refuses
//     nothing a probe could read, and with -verify_return_error it reads OpenSSL's default store,
//     the one curl, python3, redis-cli and socat are probed through. Measured on 2026-11-18.
//   - BIND's dig, delv, host, nslookup, mdig and nsupdate, which link libssl for DNS over TLS and
//     over HTTPS but give no verdict at the sites: `dig +https`, `+tls` and `+tls +tls-ca` each
//     gave "end of file" or "communications error" at every site (measured on 2026-11-18).
//   - redis-check-aof, redis-check-rdb and dnstap-read, which link libssl but read local files and
//     open no connection.
var workerTLSClients = []tlsClientSpec{
	{
		name: "node", program: "node", timeout: tlsDefaultProbeTimeout,
		command: func(tlsSite, string, string) ([]string, []string) {
			return []string{"--input-type=module", "--eval", tlsFetchProgram}, nil
		},
	},
	{
		name: "bun", program: "bun", timeout: tlsDefaultProbeTimeout,
		command: func(tlsSite, string, string) ([]string, []string) {
			return []string{"--eval", tlsFetchProgram}, nil
		},
	},
	{
		name: "omp", program: "sh", timeout: tlsDefaultProbeTimeout,
		command: func(_ tlsSite, _, invocation string) ([]string, []string) {
			return []string{"-c", "exec " + invocation + " --eval \"$1\"", "sh", tlsFetchProgram}, []string{"BUN_BE_BUN=1"}
		},
	},
	{
		name: "codegraph-node", program: "/opt/codegraph/node", timeout: tlsDefaultProbeTimeout,
		command: func(tlsSite, string, string) ([]string, []string) {
			return []string{"--input-type=module", "--eval", tlsFetchProgram}, nil
		},
	},
	{
		name: "uv", program: "uv", timeout: tlsDefaultProbeTimeout, applicationSeen: uvResponseSeen,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			return []string{"pip", "install", "--system", "--break-system-packages", "--dry-run", "--no-deps", "--no-cache", "--index-url", site.url, "legion-tls-probe-not-a-package"}, nil
		},
	},
	{
		name: "aws", program: "aws", timeout: tlsDefaultProbeTimeout, applicationSeen: awsResponseSeen,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			return []string{"sts", "get-caller-identity", "--no-sign-request", "--region", "us-east-1", "--endpoint-url", site.url}, nil
		},
	},
	// curl reads a curlrc, and sends its headers to every host it reaches, from CURL_HOME,
	// XDG_CONFIG_HOME or HOME, and where none of them holds one, from the home the passwd database
	// names for its user (curl 8.14.1, findfile in tool_findfile.c), so the HOME of its own that
	// build gives it does not keep it from the runtime user's curlrc. -q (--disable) keeps curl
	// from every curlrc, but only as its first argument. Measured on 2026-11-18 on the image of
	// d56fa735 against a local listener: with /home/legion/.curlrc holding an Authorization header,
	// this client without -q sent that header, and with a curlrc in CURL_HOME, `curl -q -s` sent
	// none while `curl -s -q` sent it. The positive controls' curl (tlsPositiveControlSpec) takes
	// -q first for the same reason.
	{
		name: "curl", program: "curl", timeout: tlsDefaultProbeTimeout,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			return []string{"-q", "--fail", "--silent", "--show-error", site.url}, nil
		},
	},
	{
		name: "python3", program: "python3", timeout: tlsDefaultProbeTimeout,
		command: func(tlsSite, string, string) ([]string, []string) {
			return []string{"-c", "import os, urllib.request; urllib.request.urlopen(os.environ['LEGION_TLS_URL']).read(1)"}, nil
		},
	},
	{
		name: "gh", program: "/usr/local/bin/gh", timeout: tlsDefaultProbeTimeout, applicationSeen: ghResponseSeen,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			return []string{"api", site.url}, []string{"GH_TOKEN=" + tlsProbeSignIn}
		},
	},
	// --no-hsts keeps wget from writing ~/.wget-hsts into the runtime user's home. wget exits 0 on
	// the control's page, so, as curl, it needs no response marker.
	{
		name: "wget", program: "wget", timeout: tlsDefaultProbeTimeout,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			return []string{"--no-verbose", "--no-hsts", "--tries=1", "--output-document=/dev/null", site.url}, nil
		},
	},
	// The PATH's git: in a pod, the /opt/legion/bin/git wrapper, which hands ls-remote to
	// /usr/bin/git unchanged. GIT_TERMINAL_PROMPT=0 keeps it from waiting on a credential prompt.
	// Its user configuration, whose http.extraHeader git sends to every host, is under its own empty
	// HOME, and no GIT_CONFIG_* variable of the pod's reaches it (tlsKeptVariables), so outside a
	// repository git reads the image's system configuration, /etc/gitconfig, and no other.
	// No site is a git repository, so git fails ls-remote at the control too, after the handshake;
	// gitSiteAnswer reads that answer.
	{
		name: "git", program: "git", timeout: tlsDefaultProbeTimeout, applicationSeen: gitSiteAnswer.MatchString,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			return []string{"ls-remote", site.url}, []string{"GIT_TERMINAL_PROMPT=0"}
		},
	},
	// go's fetch of a module path at the site's host, the fetch `go install` and `go get` make with
	// GOPROXY=direct. -C makes go change to the state directory before it reads anything, so no
	// go.mod of the probe's working directory is read (measured on 2026-11-18, go 1.27.3: -C after
	// `mod download` works, and a go.mod of the working directory that needs go 1.99 is not read),
	// and every path go writes is under that directory. GOTOOLCHAIN=local keeps it from downloading
	// a toolchain, and GOAUTH=off from running any authentication command. No site serves a Go
	// module path, so go fails at the control too; goGetAnswer reads the site's answer.
	{
		name: "go", program: "go", timeout: tlsDefaultProbeTimeout, applicationSeen: goGetAnswer.MatchString,
		command: func(site tlsSite, stateDir, _ string) ([]string, []string) {
			home := filepath.Join(stateDir, "go")
			return []string{"mod", "download", "-C", stateDir, tlsSiteURL(site).Hostname() + "/legion-tls-probe@latest"}, []string{
				"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
				"GOPATH=" + filepath.Join(home, "path"),
				"GOMODCACHE=" + filepath.Join(home, "mod"),
				"GOCACHE=" + filepath.Join(home, "cache"),
				"GOPROXY=direct",
				"GOTOOLCHAIN=local",
				"GOFLAGS=",
				"GOWORK=off",
				"GOAUTH=off",
			}
		},
	},
	// kubectl at the site as its API server. Without a credential kubectl asks for a user name
	// before it connects at all, so it signs in with the dummy token. No variable of the pod's but
	// the kept ones reaches it (tlsKeptVariables): with no KUBERNETES_SERVICE_HOST and
	// KUBERNETES_SERVICE_PORT, client-go never takes the in-cluster configuration, which would send
	// the pod's service-account token to a test site, and with no KUBECONFIG kubectl reads its
	// default, $HOME/.kube/config, a file its own empty HOME does not hold, which it reads as empty.
	// kubectl exits 0 on the control's page, so, as curl, it needs no response marker.
	{
		name: "kubectl", program: "kubectl", timeout: tlsDefaultProbeTimeout,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			return []string{"--server=" + site.url, "--token=" + tlsProbeSignIn, "get", "--raw", "/"}, nil
		},
	},
	// redis-cli's TLS connection to the site's host and port, with the host as its SNI name, the
	// way `redis-cli --tls -h <host> -p <port>` reaches a Redis server a role names. No site speaks
	// Redis, so redis-cli fails at the control too, past the handshake; redisResponseSeen reads
	// that answer. redis-cli sends REDISCLI_AUTH as AUTH to the server it reaches, here a test site
	// (measured on 2026-11-18 at example.com with a dummy value set, redis-cli 8.0.2 printed "I/O
	// error" for the AUTH before its protocol error), and no kept variable is REDISCLI_AUTH, so the
	// pod's never reaches it.
	{
		name: "redis-cli", program: "redis-cli", timeout: tlsDefaultProbeTimeout, applicationSeen: redisResponseSeen,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			host, port := tlsSiteHostPort(site)
			return []string{"--tls", "--sni", host, "-h", host, "-p", port, "PING"}, nil
		},
	},
	// gcloud's compute call, with the compute endpoint at the site. `gcloud storage` is not the
	// form because it retried for 35-41 s before it answered.
	{
		name: "gcloud", program: "gcloud", timeout: tlsDefaultProbeTimeout, optional: true, applicationSeen: gcloudResponseSeen,
		command: func(site tlsSite, stateDir, _ string) ([]string, []string) {
			return []string{"compute", "regions", "list", "--project", "legion-tls-probe"},
				append(googleCloudTLSEnvironment(stateDir, "gcloud"), "CLOUDSDK_API_ENDPOINT_OVERRIDES_COMPUTE="+site.url+"compute/v1/")
		},
	},
	// --oauth_access_token makes the SDK's bq launcher skip its own credential lookup, which asks
	// the metadata server (bin/bootstrapping/bq.py) and, with no account, refuses with "You do not
	// currently have an active account selected"; the token file still serves the `gcloud config`
	// subprocess bq runs. bq writes its error to stdout, so the client reads stdout too.
	{
		name: "bq", program: "bq", timeout: tlsBigQueryProbeTimeout, optional: true, readStdout: true, applicationSeen: bqResponseSeen,
		command: func(site tlsSite, stateDir, _ string) ([]string, []string) {
			return []string{"--oauth_access_token=" + tlsProbeSignIn, "--api=" + site.url, "ls", "--project_id=legion-tls-probe"}, googleCloudTLSEnvironment(stateDir, "bq")
		},
	},
	// One configuration directory for az's three sites, so the version fetch from
	// azcliprod.blob.core.windows.net, which az makes at its first command in a configuration
	// directory, happens once a probe. az exited 0 on the control's page, so, as curl, it needs no
	// response marker.
	{
		name: "az", program: "az", timeout: tlsDefaultProbeTimeout, optional: true,
		command: func(site tlsSite, stateDir, _ string) ([]string, []string) {
			return []string{"rest", "--method", "get", "--url", site.url, "--skip-authorization-header"}, []string{"AZURE_CONFIG_DIR=" + filepath.Join(stateDir, "az", "config")}
		},
	},
	// mise's http: backend, downloading the site as a tool, in a directory of its own per site so
	// one site's download never satisfies the next. MISE_VERBOSE prints the status line
	// miseHTTPStatus reads.
	{
		name: "mise", program: "mise", timeout: tlsDefaultProbeTimeout, optional: true, applicationSeen: miseHTTPStatus.MatchString,
		command: func(site tlsSite, stateDir, _ string) ([]string, []string) {
			home := filepath.Join(stateDir, "mise", site.name)
			return []string{"x", "http:legion-tls-probe[url=" + site.url + "]@1", "--", "true"}, []string{
				"HOME=" + home,
				"MISE_DATA_DIR=" + filepath.Join(home, "data"),
				"MISE_CACHE_DIR=" + filepath.Join(home, "cache"),
				"MISE_CONFIG_DIR=" + filepath.Join(home, "config"),
				"MISE_STATE_DIR=" + filepath.Join(home, "state"),
				"MISE_YES=1",
				"MISE_VERBOSE=1",
			}
		},
	},
	// socat's OPENSSL: address at the site's host and port. Its stdin is the probe's /dev/null, so
	// it completes the handshake, sends nothing and exits. It exits 0 on the control, so, as curl,
	// it needs no response marker, and it keeps no state.
	{
		name: "socat", program: "socat", timeout: tlsDefaultProbeTimeout, optional: true,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			host, port := tlsSiteHostPort(site)
			return []string{"-u", "-", "OPENSSL:" + host + ":" + port}, nil
		},
	},
	{
		name: "chromium", program: "chromium", timeout: tlsChromiumProbeTimeout, chromium: true,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			return chromiumTLSArgs(site), nil
		},
	},
}

// chromiumTLSArgs returns the Chromium arguments that load one site with every other host name
// unresolvable. Chromium's certificate verifier fetches the AIA issuer URLs of a chain it cannot
// anchor before it refuses it, and Chromium has no switch that turns those fetches off; emSign's AIA
// host, repository.emsign.com, stalls or resets its connections, and a stalled fetch held the emSign
// load past the probe's limit. With the AIA host unresolvable, each fetch fails at once and the verdict
// comes from the served chain and Chromium's own trust store alone. That is the verdict to prove:
// the positive controls show that each test site serves every certificate up to its pinned root's
// subject, and the sites' AIA URLs hold only self-signed roots, which a path can end at only as
// anchors of the local trust store. `^NOTFOUND` is the rule's token for "fail with
// ERR_NAME_NOT_RESOLVED" (net/dns/mapped_host_resolver.h). A proxy in env is a host name or address
// this rule also leaves unresolvable, so behind one every load fails as unavailable, never as a
// refusal.
func chromiumTLSArgs(site tlsSite) []string {
	return []string{"--headless", "--no-sandbox", "--host-resolver-rules=MAP * ^NOTFOUND, EXCLUDE " + tlsSiteURL(site).Hostname(), "--dump-dom", site.url}
}

// tlsSiteURL is site's URL, parsed. Every tlsSites URL is an absolute https URL, which url.Parse
// reads without error.
func tlsSiteURL(site tlsSite) *url.URL {
	parsed, _ := url.Parse(site.url)
	return parsed
}

// tlsSiteHostPort is the host and port of site's URL, the port 443 where the URL names none, as
// no tlsSites URL does.
func tlsSiteHostPort(site tlsSite) (host, port string) {
	address := tlsSiteURL(site)
	port = address.Port()
	if port == "" {
		port = "443"
	}
	return address.Hostname(), port
}

// imageTLSClients returns the image's TLS-capable programs. The gh wrapper manages worker tokens,
// so the probe runs the real gh binary it wraps; no probe uses an operator token. stateDir is where
// every client has its home and the clients that keep state keep it, and the directory go changes
// to (newTLSStateDir).
func imageTLSClients(invocation string, env []string, workDir, stateDir string) []tlsClient {
	clients := make([]tlsClient, 0, len(workerTLSClients))
	for _, spec := range workerTLSClients {
		clients = append(clients, spec.build(invocation, env, workDir, stateDir))
	}
	return clients
}

func workerTLSClientNames() []string {
	names := make([]string, 0, len(workerTLSClients))
	for _, spec := range workerTLSClients {
		names = append(names, spec.name)
	}
	return names
}

// newImageTLSClients is a variable so the command's image-environment tests can use fake clients.
var newImageTLSClients = imageTLSClients

// tlsAnyMarker is a client's applicationSeen that reads the site's answer in output holding any of
// markers.
func tlsAnyMarker(markers ...string) func(string) bool {
	return func(detail string) bool {
		return slices.ContainsFunc(markers, func(marker string) bool { return strings.Contains(detail, marker) })
	}
}

var uvResponseSeen = tlsAnyMarker("No solution found when resolving dependencies")

var awsResponseSeen = tlsAnyMarker("An error occurred", "InvalidResponse", "invalid XML", "ResponseParserError")

var ghResponseSeen = tlsAnyMarker("HTTP ")

// gcloudResponseSeen reads an HTTP answer from the site in gcloud's compute call: the site's page
// is not the compute API's, so gcloud fails on it, but only after a handshake it accepted. An SDK
// that accepted the emSign chain answered "Error decoding response" for that site's page, and
// the stock SDK 589.0.0, which trusts its own bundled roots, crashed on the page instead
// ("gcloud crashed (JSONDecodeError): Expecting value", measured 2026-11-18). A JSONDecodeError
// is a body the site served, so it reads as accepted, never as unavailable.
var gcloudResponseSeen = tlsAnyMarker("Some requests did not succeed", "Error decoding response", "JSONDecodeError")

// bqResponseSeen reads an HTTP answer in bq's fetch of its discovery document: a status the site
// answered ("Got 404 response from discovery url"), a page bq fetched and could not use as the
// document (platform/bq/clients/bigquery_client.py, _LoadDiscoveryDocumentUrl), or a page it could
// not parse as JSON at all, which bq 2.1.40 of the stock SDK reported as "a bug in the BigQuery
// CLI" with "json.decoder.JSONDecodeError" in its trace at the emSign site (measured 2026-11-18).
var bqResponseSeen = tlsAnyMarker("response from discovery url", "discovery doc", "contains no resources", "JSONDecodeError")

// miseHTTPStatus is the line MISE_VERBOSE prints for a request the site answered
// ("DEBUG GET https://example.com/ 200 OK"); a request that failed prints its URL with no status.
var miseHTTPStatus = regexp.MustCompile(`\bGET \S+ [1-5][0-9][0-9]\b`)

// gitSiteAnswer is how git's ls-remote names a site that answered but serves no repository: a
// 404 ("fatal: repository 'https://example.com/' not found", the control) or a page that is no
// ref advertisement ("fatal: https://<host>/info/refs not valid: is this a git repository?", the
// emSign and AAA sites' answer, measured on 2026-11-18 with GIT_SSL_NO_VERIFY=true). A refused
// chain is "unable to access" the URL instead. Reading both answers keeps a git that accepted a
// compromised chain reported as accepting it rather than as unavailable.
var gitSiteAnswer = regexp.MustCompile(`repository '\S+' not found|/info/refs not valid`)

// goGetAnswer is how go names the page a site served at its go-get=1 fetch: its status
// ("reading https://example.com/legion-tls-probe?go-get=1: 404 Not Found", the control), or a page
// it read and found no go-import meta tags in ("parse https://<host>/legion-tls-probe?go-get=1: no
// go-import meta tags ()", the emSign and AAA sites' answer, measured on 2026-11-18 with
// GOINSECURE=*). A fetch that failed names the URL in quotes and then its error. Reading both
// answers keeps a go that accepted a compromised chain reported as accepting it rather than as
// unavailable.
var goGetAnswer = regexp.MustCompile(`go-get=1: (?:[1-5][0-9][0-9]\b|no go-import meta tags)`)

// redisResponseSeen reads an answer redis-cli got past the handshake. The control answers its
// PING with HTTP, which redis-cli names a reply of no type it knows ("Error: Protocol error, got
// "H" as reply type byte"), and a compromised site whose chain it accepted closes the connection
// ("Error: Server closed the connection", measured on 2026-11-18 with --insecure). A refused chain
// is "Could not connect to Redis at <host>:443: SSL_connect failed: certificate verify failed"
// instead.
var redisResponseSeen = tlsAnyMarker("Protocol error", "Server closed the connection")
