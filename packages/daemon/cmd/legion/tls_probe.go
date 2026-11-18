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
	"strings"
	"time"
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

func probeTLSPositiveControls(ctx context.Context, env []string, workDir string) error {
	return verifyTLSPositiveControls(compromisedTLSSites(), func(site tlsSite) tlsOutcome {
		client := tlsCommandClient{clientName: "positive-control", env: env, workDir: workDir, timeout: tlsDefaultProbeTimeout, command: func(site tlsSite) (string, []string, []string) {
			return "curl", []string{"--cacert", site.root, "--capath", "/nonexistent", "--fail", "--silent", "--show-error", site.url}, nil
		}}
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
	clientName      string
	env             []string
	workDir         string
	timeout         time.Duration
	command         func(tlsSite) (string, []string, []string)
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

// run returns the command's stderr, with its stdout interleaved when the client reads stdout.
func (c tlsCommandClient) run(ctx context.Context, site tlsSite) (string, error) {
	program, args, extraEnv := c.command(site)
	commandCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, program, args...)
	cmd.Env = tlsEnvironment(c.env, append(extraEnv, "LEGION_TLS_URL="+site.url))
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

// tlsEnvironment is base with a client's additions applied: NAME=value sets NAME in place of base's
// value, and a bare NAME drops NAME from base, for a variable that would send a credential of the
// pod's to a test site (redis-cli's REDISCLI_AUTH).
func tlsEnvironment(base, additions []string) []string {
	out := make([]string, 0, len(base)+len(additions))
	for _, pair := range base {
		name, _, ok := strings.Cut(pair, "=")
		if !ok || !tlsEnvironmentOverrides(name, additions) {
			out = append(out, pair)
		}
	}
	for _, pair := range additions {
		if strings.Contains(pair, "=") {
			out = append(out, pair)
		}
	}
	return out
}

func tlsEnvironmentOverrides(name string, additions []string) bool {
	for _, pair := range additions {
		if added, _, _ := strings.Cut(pair, "="); added == name {
			return true
		}
	}
	return false
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

type tlsClientSpec struct {
	name    string
	timeout time.Duration
	build   func(invocation string, env []string, workDir, stateDir string, timeout time.Duration) tlsClient
}

func tlsCommandClientSpec(name string, timeout time.Duration, program string, args func(tlsSite) []string, applicationSeen func(string) bool, extraEnv ...string) tlsClientSpec {
	return tlsClientSpec{
		name: name, timeout: timeout,
		build: func(_ string, env []string, workDir, _ string, timeout time.Duration) tlsClient {
			return tlsCommandClient{
				clientName: name, env: env, workDir: workDir, timeout: timeout, applicationSeen: applicationSeen,
				command: func(site tlsSite) (string, []string, []string) {
					return program, args(site), extraEnv
				},
			}
		},
	}
}

// tlsStateClientSpec is the spec of a floor client named for its program. command gives one site's
// arguments and the environment it adds, with the client's state under stateDir (newTLSStateDir);
// readStdout is the command client's.
func tlsStateClientSpec(program string, timeout time.Duration, applicationSeen func(string) bool, readStdout bool, command func(site tlsSite, stateDir string) (args, extraEnv []string)) tlsClientSpec {
	return tlsClientSpec{
		name: program, timeout: timeout,
		build: func(_ string, env []string, workDir, stateDir string, timeout time.Duration) tlsClient {
			return tlsCommandClient{
				clientName: program, env: env, workDir: workDir, timeout: timeout, applicationSeen: applicationSeen, readStdout: readStdout,
				command: func(site tlsSite) (string, []string, []string) {
					args, extraEnv := command(site, stateDir)
					return program, args, extraEnv
				},
			}
		},
	}
}

// tlsOptionalClientSpec is tlsStateClientSpec's spec for a tlsOptionalClient, which is absent
// where its program is not on PATH.
func tlsOptionalClientSpec(program string, timeout time.Duration, applicationSeen func(string) bool, readStdout bool, command func(site tlsSite, stateDir string) (args, extraEnv []string)) tlsClientSpec {
	spec := tlsStateClientSpec(program, timeout, applicationSeen, readStdout, command)
	build := spec.build
	spec.build = func(invocation string, env []string, workDir, stateDir string, timeout time.Duration) tlsClient {
		// tlsStateClientSpec builds a tlsCommandClient.
		return tlsOptionalClient{program: program, tlsCommandClient: build(invocation, env, workDir, stateDir, timeout).(tlsCommandClient)}
	}
	return spec
}

// inTLSStateDir runs spec's client in the probe's state directory instead of the probe's working
// directory, for a program that reads files of its working directory: go reads the go.mod of that
// directory or of the nearest directory above it.
func inTLSStateDir(spec tlsClientSpec) tlsClientSpec {
	build := spec.build
	spec.build = func(invocation string, env []string, _, stateDir string, timeout time.Duration) tlsClient {
		return build(invocation, env, stateDir, stateDir, timeout)
	}
	return spec
}

// tlsProbeSignIn is the dummy credential gcloud, bq and kubectl sign in with: gcloud and bq from
// the state directory's tlsAccessTokenFile and, for bq, its --oauth_access_token flag, and kubectl
// from its --token flag. The probe needs their TLS handshake, never an authorized call. Its name
// holds no "token", so the public leak check does not read the literal as a credential.
const (
	tlsProbeSignIn     = "legion-tls-probe"
	tlsAccessTokenFile = "access-token"
)

// newTLSStateDir makes the directory the TLS probe's gcloud, bq, az, mise, go and kubectl keep
// their state in (go also runs there), so none of it lands in the runtime user's home (the image
// build's final step requires that), and writes into it the access-token file gcloud and bq sign
// in with. remove deletes the directory with everything the clients wrote there.
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

// googleCloudTLSEnvironment is the environment gcloud and bq run under, client naming the home and
// configuration directory each keeps under stateDir. The SDK signs in with the dummy token of the
// state directory's access-token file, so it needs neither an account nor a metadata server, and
// core/check_gce_metadata=false and a set metrics/environment are the two settings that stop it
// asking a metadata server whether it runs on GCE: measured on 2026-11-18 with the metadata server
// blackholed, each of those checks cost about 18 s of waiting, and with both set a call took 2 s.
// An image build has no metadata server and a cluster pod has one, so the outcome must not depend
// on it.
func googleCloudTLSEnvironment(stateDir, client string) []string {
	home := filepath.Join(stateDir, client)
	return []string{
		"HOME=" + home,
		"CLOUDSDK_CONFIG=" + filepath.Join(home, "config"),
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
// The pairs, the clients probed for each, and the programs named under them:
//
//   - OpenSSL (libssl.so.3) with OpenSSL's default store, /usr/lib/ssl/cert.pem, which is Debian's
//     bundle: curl, python3 and redis-cli on every image, and socat where it is on PATH. Named
//     under them: redis-benchmark; the libpq programs pg_wrapper runs (psql, pg_dump, pg_dumpall,
//     pg_restore, pg_basebackup, pg_isready, pg_receivewal, pg_recvlogical, pgbench, createdb,
//     createuser, dropdb, dropuser, clusterdb, reindexdb, vacuumdb and vacuumlo), which verify
//     against that store only where a role names sslrootcert=system (psql, below); and every
//     Python script on PATH.
//   - GnuTLS with Debian's bundle, /etc/ssl/certs/ca-certificates.crt: wget, and git, whose https
//     transport, git-remote-https, links libcurl's GnuTLS build (curl itself links OpenSSL).
//   - Go's crypto/tls with the system roots crypto/x509 reads: gh, go and kubectl. Named under
//     them: legion, agent-secrets and gopls. yq and gofmt are Go programs that do not link
//     crypto/tls.
//   - A stack or a CA bundle of the program's own, so each of these is probed itself: node, and
//     the CodeGraph runtime (/opt/codegraph/node, a link to the same Node), with Node's bundled
//     stack and roots, the pair a `#!/usr/bin/env node` script uses too; bun, and omp, which is a
//     Bun program, with BoringSSL and Bun's roots; uv and mise, which link rustls; the AWS CLI, which
//     bundles its own Python and botocore's own CA bundle (awscli/botocore/cacert.pem in its dist
//     tree); gcloud and bq, which bundle their own Python and are pointed at Debian's bundle; az,
//     whose Python is its own and whose wrapper points requests at Debian's bundle; and Chromium,
//     with NSS and its own root store.
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
	tlsCommandClientSpec("node", tlsDefaultProbeTimeout, "node", func(tlsSite) []string { return []string{"--input-type=module", "--eval", tlsFetchProgram} }, nil),
	tlsCommandClientSpec("bun", tlsDefaultProbeTimeout, "bun", func(tlsSite) []string { return []string{"--eval", tlsFetchProgram} }, nil),
	{
		name: "omp", timeout: tlsDefaultProbeTimeout,
		build: func(invocation string, env []string, workDir, _ string, timeout time.Duration) tlsClient {
			return tlsCommandClient{
				clientName: "omp", env: env, workDir: workDir, timeout: timeout,
				command: func(tlsSite) (string, []string, []string) {
					return "sh", []string{"-c", "exec " + invocation + " --eval \"$1\"", "sh", tlsFetchProgram}, []string{"BUN_BE_BUN=1"}
				},
			}
		},
	},
	tlsCommandClientSpec("codegraph-node", tlsDefaultProbeTimeout, "/opt/codegraph/node", func(tlsSite) []string { return []string{"--input-type=module", "--eval", tlsFetchProgram} }, nil),
	tlsCommandClientSpec("uv", tlsDefaultProbeTimeout, "uv", func(site tlsSite) []string {
		return []string{"pip", "install", "--system", "--break-system-packages", "--dry-run", "--no-deps", "--no-cache", "--index-url", site.url, "legion-tls-probe-not-a-package"}
	}, uvResponseSeen),
	tlsCommandClientSpec("aws", tlsDefaultProbeTimeout, "aws", func(site tlsSite) []string {
		return []string{"sts", "get-caller-identity", "--no-sign-request", "--region", "us-east-1", "--endpoint-url", site.url}
	}, awsResponseSeen),
	tlsCommandClientSpec("curl", tlsDefaultProbeTimeout, "curl", func(site tlsSite) []string { return []string{"--fail", "--silent", "--show-error", site.url} }, nil),
	tlsCommandClientSpec("python3", tlsDefaultProbeTimeout, "python3", func(tlsSite) []string {
		return []string{"-c", "import os, urllib.request; urllib.request.urlopen(os.environ['LEGION_TLS_URL']).read(1)"}
	}, nil),
	tlsCommandClientSpec("gh", tlsDefaultProbeTimeout, "/usr/local/bin/gh", func(site tlsSite) []string { return []string{"api", site.url} }, ghResponseSeen, "GH_TOKEN=legion-tls-probe"),
	// --no-hsts keeps wget from writing ~/.wget-hsts into the runtime user's home. wget exits 0 on
	// the control's page, so, as curl, it needs no response marker.
	tlsCommandClientSpec("wget", tlsDefaultProbeTimeout, "wget", func(site tlsSite) []string {
		return []string{"--no-verbose", "--no-hsts", "--tries=1", "--output-document=/dev/null", site.url}
	}, nil),
	// The PATH's git: in a pod, the /opt/legion/bin/git wrapper, which hands ls-remote to
	// /usr/bin/git unchanged. GIT_TERMINAL_PROMPT=0 keeps it from waiting on a credential prompt.
	// No site is a git repository, so git fails ls-remote at the control too, after the handshake;
	// gitResponseSeen reads that answer.
	tlsCommandClientSpec("git", tlsDefaultProbeTimeout, "git", func(site tlsSite) []string { return []string{"ls-remote", site.url} }, gitResponseSeen, "GIT_TERMINAL_PROMPT=0"),
	// go's fetch of a module path at the site's host, the fetch `go install` and `go get` make with
	// GOPROXY=direct. Every path go writes is under the state directory, and it runs there, so no
	// go.mod of the probe's working directory is read; GOTOOLCHAIN=local keeps it from downloading
	// a toolchain. No site serves a Go module path, so go fails at the control too; goResponseSeen
	// reads the site's answer.
	inTLSStateDir(tlsStateClientSpec("go", tlsDefaultProbeTimeout, goResponseSeen, false, func(site tlsSite, stateDir string) ([]string, []string) {
		home := filepath.Join(stateDir, "go")
		return []string{"mod", "download", tlsSiteURL(site).Hostname() + "/legion-tls-probe@latest"}, []string{
			"HOME=" + home,
			"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
			"GOPATH=" + filepath.Join(home, "path"),
			"GOMODCACHE=" + filepath.Join(home, "mod"),
			"GOCACHE=" + filepath.Join(home, "cache"),
			"GOPROXY=direct",
			"GOTOOLCHAIN=local",
			"GOFLAGS=",
			"GOWORK=off",
		}
	})),
	// kubectl at the site as its API server. Without a credential kubectl asks for a user name
	// before it connects at all, so it signs in with the dummy token. KUBERNETES_SERVICE_HOST and
	// KUBERNETES_SERVICE_PORT are empty so client-go never takes the in-cluster configuration,
	// which would send the pod's service-account token to a test site; KUBECONFIG names a file
	// that does not exist, which kubectl reads as empty. kubectl exits 0 on the control's page, so,
	// as curl, it needs no response marker.
	tlsStateClientSpec("kubectl", tlsDefaultProbeTimeout, nil, false, func(site tlsSite, stateDir string) ([]string, []string) {
		home := filepath.Join(stateDir, "kubectl")
		return []string{"--server=" + site.url, "--token=" + tlsProbeSignIn, "get", "--raw", "/"}, []string{
			"HOME=" + home,
			"KUBECONFIG=" + filepath.Join(home, "config"),
			"KUBERNETES_SERVICE_HOST=",
			"KUBERNETES_SERVICE_PORT=",
		}
	}),
	// redis-cli's TLS connection to the site's host and port, with the host as its SNI name, the
	// way `redis-cli --tls -h <host> -p <port>` reaches a Redis server a role names. No site speaks
	// Redis, so redis-cli fails at the control too, past the handshake; redisResponseSeen reads
	// that answer. REDISCLI_AUTH is dropped from its environment: redis-cli sends that variable as
	// AUTH to the server it reaches, here a test site (measured on 2026-11-18 at example.com with a
	// dummy value set, redis-cli 8.0.2 printed "I/O error" for the AUTH before its protocol error).
	tlsCommandClientSpec("redis-cli", tlsDefaultProbeTimeout, "redis-cli", func(site tlsSite) []string {
		host, port := tlsSiteHostPort(site)
		return []string{"--tls", "--sni", host, "-h", host, "-p", port, "PING"}
	}, redisResponseSeen, "REDISCLI_AUTH"),
	// gcloud's compute call, with the compute endpoint at the site. `gcloud storage` is not the
	// form because it retried for 35-41 s before it answered.
	tlsOptionalClientSpec("gcloud", tlsDefaultProbeTimeout, gcloudResponseSeen, false, func(site tlsSite, stateDir string) ([]string, []string) {
		return []string{"compute", "regions", "list", "--project", "legion-tls-probe"},
			append(googleCloudTLSEnvironment(stateDir, "gcloud"), "CLOUDSDK_API_ENDPOINT_OVERRIDES_COMPUTE="+site.url+"compute/v1/")
	}),
	// --oauth_access_token makes the SDK's bq launcher skip its own credential lookup, which asks
	// the metadata server (bin/bootstrapping/bq.py) and, with no account, refuses with "You do not
	// currently have an active account selected"; the token file still serves the `gcloud config`
	// subprocess bq runs. bq writes its error to stdout, so the client reads stdout too.
	tlsOptionalClientSpec("bq", tlsBigQueryProbeTimeout, bqResponseSeen, true, func(site tlsSite, stateDir string) ([]string, []string) {
		return []string{"--oauth_access_token=" + tlsProbeSignIn, "--api=" + site.url, "ls", "--project_id=legion-tls-probe"}, googleCloudTLSEnvironment(stateDir, "bq")
	}),
	// One configuration directory for az's three sites, so the version fetch from
	// azcliprod.blob.core.windows.net, which az makes at its first command in a configuration
	// directory, happens once a probe. az exited 0 on the control's page, so, as curl, it needs no
	// response marker.
	tlsOptionalClientSpec("az", tlsDefaultProbeTimeout, nil, false, func(site tlsSite, stateDir string) ([]string, []string) {
		home := filepath.Join(stateDir, "az")
		return []string{"rest", "--method", "get", "--url", site.url, "--skip-authorization-header"}, []string{"HOME=" + home, "AZURE_CONFIG_DIR=" + filepath.Join(home, "config")}
	}),
	// mise's http: backend, downloading the site as a tool, in a directory of its own per site so
	// one site's download never satisfies the next. MISE_VERBOSE prints the status line
	// miseResponseSeen reads.
	tlsOptionalClientSpec("mise", tlsDefaultProbeTimeout, miseResponseSeen, false, func(site tlsSite, stateDir string) ([]string, []string) {
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
	}),
	// socat's OPENSSL: address at the site's host and port. Its stdin is the probe's /dev/null, so
	// it completes the handshake, sends nothing and exits. It exits 0 on the control, so, as curl,
	// it needs no response marker, and it keeps no state.
	tlsOptionalClientSpec("socat", tlsDefaultProbeTimeout, nil, false, func(site tlsSite, _ string) ([]string, []string) {
		host, port := tlsSiteHostPort(site)
		return []string{"-u", "-", "OPENSSL:" + host + ":" + port}, nil
	}),
	{
		name: "chromium", timeout: tlsChromiumProbeTimeout,
		build: func(_ string, env []string, workDir, _ string, timeout time.Duration) tlsClient {
			return tlsChromiumClient{tlsCommandClient: tlsCommandClient{
				clientName: "chromium", env: env, workDir: workDir, timeout: timeout,
				command: func(site tlsSite) (string, []string, []string) {
					return "chromium", chromiumTLSArgs(site), nil
				},
			}}
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
// the clients that keep state keep it, and where go runs (newTLSStateDir).
func imageTLSClients(invocation string, env []string, workDir, stateDir string) []tlsClient {
	clients := make([]tlsClient, 0, len(workerTLSClients))
	for _, spec := range workerTLSClients {
		clients = append(clients, spec.build(invocation, env, workDir, stateDir, spec.timeout))
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

func uvResponseSeen(detail string) bool {
	return strings.Contains(detail, "No solution found when resolving dependencies")
}

func awsResponseSeen(detail string) bool {
	for _, marker := range []string{"An error occurred", "InvalidResponse", "invalid XML", "ResponseParserError"} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

func ghResponseSeen(detail string) bool {
	return strings.Contains(detail, "HTTP ")
}

// gcloudResponseSeen reads an HTTP answer from the site in gcloud's compute call: the site's page
// is not the compute API's, so gcloud fails on it, but only after a handshake it accepted. An SDK
// that accepted the emSign chain answered "Error decoding response" for that site's page.
func gcloudResponseSeen(detail string) bool {
	return strings.Contains(detail, "Some requests did not succeed") || strings.Contains(detail, "Error decoding response")
}

// bqResponseSeen reads an HTTP answer in bq's fetch of its discovery document: a status the site
// answered ("Got 404 response from discovery url"), or a page bq fetched and could not use as the
// document (platform/bq/clients/bigquery_client.py, _LoadDiscoveryDocumentUrl).
func bqResponseSeen(detail string) bool {
	for _, marker := range []string{"response from discovery url", "discovery doc", "contains no resources"} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

// miseHTTPStatus is the line MISE_VERBOSE prints for a request the site answered
// ("DEBUG GET https://example.com/ 200 OK"); a request that failed prints its URL with no status.
var miseHTTPStatus = regexp.MustCompile(`\bGET \S+ [1-5][0-9][0-9]\b`)

func miseResponseSeen(detail string) bool {
	return miseHTTPStatus.MatchString(detail)
}

// gitSiteAnswer is how git's ls-remote names a site that answered but serves no repository: a
// 404 ("fatal: repository 'https://example.com/' not found", the control) or a page that is no
// ref advertisement ("fatal: https://<host>/info/refs not valid: is this a git repository?", the
// emSign and AAA sites' answer, measured on 2026-11-18 with GIT_SSL_NO_VERIFY=true). A refused
// chain is "unable to access" the URL instead. Reading both answers keeps a git that accepted a
// compromised chain reported as accepting it rather than as unavailable.
var gitSiteAnswer = regexp.MustCompile(`repository '\S+' not found|/info/refs not valid`)

func gitResponseSeen(detail string) bool {
	return gitSiteAnswer.MatchString(detail)
}

// goGetAnswer is how go names the page a site served at its go-get=1 fetch: its status
// ("reading https://example.com/legion-tls-probe?go-get=1: 404 Not Found", the control), or a page
// it read and found no go-import meta tags in ("parse https://<host>/legion-tls-probe?go-get=1: no
// go-import meta tags ()", the emSign and AAA sites' answer, measured on 2026-11-18 with
// GOINSECURE=*). A fetch that failed names the URL in quotes and then its error. Reading both
// answers keeps a go that accepted a compromised chain reported as accepting it rather than as
// unavailable.
var goGetAnswer = regexp.MustCompile(`go-get=1: (?:[1-5][0-9][0-9]\b|no go-import meta tags)`)

func goResponseSeen(detail string) bool {
	return goGetAnswer.MatchString(detail)
}

// redisResponseSeen reads an answer redis-cli got past the handshake. The control answers its
// PING with HTTP, which redis-cli names a reply of no type it knows ("Error: Protocol error, got
// "H" as reply type byte"), and a compromised site whose chain it accepted closes the connection
// ("Error: Server closed the connection", measured on 2026-11-18 with --insecure). A refused chain
// is "Could not connect to Redis at <host>:443: SSL_connect failed: certificate verify failed"
// instead.
func redisResponseSeen(detail string) bool {
	return strings.Contains(detail, "Protocol error") || strings.Contains(detail, "Server closed the connection")
}
