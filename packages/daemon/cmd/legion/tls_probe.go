package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjawhar/legion/daemon/internal/redact"
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

// tlsSite is one site the probe holds the clients to. The probe's own server (tlsServer) serves
// five, which no public host or name reaches: the fixture emSign and AAA chains, which every client
// must refuse; the control, which every client must accept; and the public test sites' own chains,
// vendored, which every client that checks a chain during the handshake must refuse before the
// server's signature. The fixture chains show a store holding no root of the distrusted roots'
// names; the vendored chains show the refusal the public sites draw, which for node, bun, omp,
// CodeGraph's node and uv is a list keyed to those chains' own certificates and no fixture can
// draw (newTLSFixture). The clients that check a chain only after the handshake are held
// to the public test sites instead, which fail the probe only where a client accepts their chain:
// any outcome there but a verdict is unknown, and fails nothing (probeTLSClients).
type tlsSite struct {
	name, url string
	// root is the root the site's positive control holds its chain to: the fixture root for a
	// fixture chain, the CA-published root for a vendored chain or a public site.
	root    string
	control bool
	// public marks a public test site, which the clients that check a chain after the handshake
	// are held to, and every client where its vendored chain failed its positive control
	// (runTLSProbe).
	public bool
	// vendored marks a public site's own chain, served by the probe's server with a key of its own
	// (tlsServer): a client that accepts it fails the server's signature (tlsSignatureFailure).
	vendored bool
	// unasked is why no client asks the site: a public site whose positive control did not accept
	// its chain (probePublicTLSSites), or a vendored chain whose positive control did not get past
	// it to the server's signature (probeVendoredTLSSites); empty for a site the clients ask.
	unasked string
	// via is the address of the probe's server, for a site it serves; empty for a public site.
	via string
	// trust is the CA file each client is given to trust at the site: the control's root, at the
	// control alone.
	trust string
	// seen counts the TLS handshakes the probe's server began for a site it serves.
	seen *atomic.Int64
	// clients names the clients held to the site, every client where it is nil (runTLSProbe).
	clients []string
}

// The public test sites, with the CA-published roots their chains reach. These roots are records
// from repository.emsign.com/certs/emSignECCRootCAG3.crt
// (86:A1:EC:BA:08:9C:4A:8D:3B:BE:27:34:C6:12:BA:34:1D:81:3E:04:3C:F9:E8:A8:62:CD:5C:57:A3:6B:BE:6B)
// and crt.sectigo.com/AAACertificateServices.crt
// (D7:A7:A0:FB:5D:7E:27:31:D7:71:E9:48:4E:BC:DE:F7:1D:5F:0C:3E:0A:29:48:78:2B:C8:3E:E0:EA:69:9E:F4).
var (
	publicEmSignTLSSite = tlsSite{name: "public emSign", url: "https://" + tlsEmSignHost + "/", root: "/usr/share/legion/tls-roots/emsign-ecc-root-ca-g3.pem", public: true}
	publicAAATLSSite    = tlsSite{name: "public AAA", url: "https://" + tlsAAAHost + "/", root: "/usr/share/legion/tls-roots/aaa-certificate-services.pem", public: true}
	publicTLSSites      = []tlsSite{publicEmSignTLSSite, publicAAATLSSite}
)

type tlsStatus string

const (
	tlsCertificateRefused tlsStatus = "refused"
	tlsConnectionAccepted tlsStatus = "accepted"
	tlsNetworkUnavailable tlsStatus = "unavailable"
	tlsClientAbsent       tlsStatus = "absent"  // an optional client whose program is not on PATH (tlsOptionalClient)
	tlsPublicUnknown      tlsStatus = "unknown" // a public site that gave no verdict, or that no client asks
)

type tlsOutcome struct {
	status tlsStatus
	detail string
	// signatureFailed marks an outcome that failed a signature the chain should have carried
	// (tlsSignatureFailure): at a fixture chain, a root of the chain's root's name in the client's
	// own store; at a vendored chain, the server's, which the probe cannot make. It is read from the
	// client's whole output, of which detail may keep only the ends.
	signatureFailed bool
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

// fixtureTLSSites are the sites of sites that serve a fixture chain the clients must refuse: no
// control, vendored chain or public site.
func fixtureTLSSites(sites []tlsSite) []tlsSite {
	var fixtures []tlsSite
	for _, site := range sites {
		if !site.control && !site.vendored && !site.public {
			fixtures = append(fixtures, site)
		}
	}
	return fixtures
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

// verifyTLSPositiveControls holds the probe's own server to the fixture chains it serves, before
// any client is held to them: each fixture chain must reach its fixture root, so a refusal there is
// a refusal of that root, and the control's chain must be refused with that root alone, so the
// acceptance proves the root, not a client that accepts anything. A failure is the probe's own and
// fails it. The vendored chains have positive controls of their own (probeVendoredTLSSites), which
// mark a chain unasked rather than fail the probe, since a chain that has expired is no fault of the
// image's.
func verifyTLSPositiveControls(sites []tlsSite, probe func(tlsSite) tlsOutcome) error {
	var failures []string
	control := slices.IndexFunc(sites, func(site tlsSite) bool { return site.control })
	for _, site := range fixtureTLSSites(sites) {
		if outcome := probe(site); outcome.status != tlsConnectionAccepted {
			failures = append(failures, fmt.Sprintf("%s positive control at %s did not accept its root: %s", site.name, site.url, outcome.detail))
		}
		if control < 0 {
			failures = append(failures, fmt.Sprintf("%s positive control has no control site to hold its root's isolation to", site.name))
			continue
		}
		isolation := sites[control]
		isolation.root, isolation.trust = site.root, ""
		if outcome := probe(isolation); outcome.status != tlsCertificateRefused {
			failures = append(failures, fmt.Sprintf("%s positive control isolation at %s was %s: %s", site.name, isolation.url, outcome.status, outcome.detail))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("worker TLS probe cannot verify test chains: %s", strings.Join(failures, "; "))
	}
	return nil
}

// tlsPositiveControlSpec is the positive controls' curl. It is built through tlsClientSpec.build,
// and takes -q first for the reason at the curl spec (workerTLSClients).
var tlsPositiveControlSpec = tlsClientSpec{
	name: "positive-control", program: "curl", timeout: tlsDefaultProbeTimeout,
	command: func(site tlsSite, _, _ string) ([]string, []string) {
		return []string{"-q", "--cacert", site.root, "--capath", "/nonexistent", "--fail", "--silent", "--show-error", site.url}, nil
	},
}

// probeTLSPositiveControls runs verifyTLSPositiveControls over sites, the probe's server's, with
// the positive controls' curl under env, its state in stateDir.
func probeTLSPositiveControls(ctx context.Context, env []string, stateDir string, sites []tlsSite) error {
	client := tlsPositiveControlSpec.build("", env, stateDir)
	return verifyTLSPositiveControls(sites, func(site tlsSite) tlsOutcome {
		return client.probe(ctx, site)
	})
}

var newTLSPositiveControls = probeTLSPositiveControls

// probeVendoredTLSSites returns the vendored sites, each marked unasked where its positive control
// failed: the positive controls' curl under env, given the CA-published root alone, must get past
// the vendored chain to the server's signature, which the probe cannot make (tlsSignatureFailure),
// and must refuse the control's chain with that root. A chain that fails, as it does once its leaf
// has expired, cannot show a client refusing that root, so no client is held to it, and its clients
// are held to the public site instead (runTLSProbe).
func probeVendoredTLSSites(ctx context.Context, env []string, stateDir string, sites []tlsSite) []tlsSite {
	client := tlsPositiveControlSpec.build("", env, stateDir)
	control := slices.IndexFunc(sites, func(site tlsSite) bool { return site.control })
	var vendored []tlsSite
	for _, site := range sites {
		if !site.vendored {
			continue
		}
		if outcome := client.probe(ctx, site); !outcome.signatureFailed {
			site.unasked = fmt.Sprintf("its positive control was %s, where it must get past the chain to the server's signature: %s", outcome.status, outcome.detail)
		} else if control >= 0 {
			isolation := sites[control]
			isolation.root, isolation.trust = site.root, ""
			if outcome := client.probe(ctx, isolation); outcome.status != tlsCertificateRefused {
				site.unasked = fmt.Sprintf("its positive control's isolation at %s was %s: %s", isolation.url, outcome.status, outcome.detail)
			}
		}
		vendored = append(vendored, site)
	}
	return vendored
}

var newVendoredTLSSites = probeVendoredTLSSites

// probePublicTLSSites returns the public sites, each marked unasked where its positive control,
// the positive controls' curl under env holding it to its CA-published root, did not accept its
// chain: a site that cannot be reached, or that no longer serves the chain the clients must refuse,
// cannot show a client trusting that root, so no client asks it.
func probePublicTLSSites(ctx context.Context, env []string, stateDir string) []tlsSite {
	client := tlsPositiveControlSpec.build("", env, stateDir)
	sites := slices.Clone(publicTLSSites)
	for index, site := range sites {
		if outcome := client.probe(ctx, site); outcome.status != tlsConnectionAccepted {
			sites[index].unasked = fmt.Sprintf("its positive control was %s: %s", outcome.status, outcome.detail)
		}
	}
	return sites
}

var newPublicTLSSites = probePublicTLSSites

// tlsSiteLine is the line that says whether the clients are held to a vendored or public site.
// One no client is held to is UNKNOWN: it says why, and never reads as verified.
func tlsSiteLine(site tlsSite) string {
	if site.unasked != "" {
		return fmt.Sprintf("probe-image: UNKNOWN: the %s chain at %s: %s", site.name, tlsSiteURL(site).Hostname(), site.unasked)
	}
	return fmt.Sprintf("probe-image: TLS %s: chain verified", site.name)
}

// tlsUnknownLine is the line that names a client the probe could not hold to a public site: the
// client, the site's host and why. The build passes, and this line, in the build's log beside the
// probe's OK line, says what it did not verify.
func tlsUnknownLine(observation tlsObservation) string {
	return fmt.Sprintf("probe-image: UNKNOWN: %s against %s: %s", observation.client, tlsSiteURL(observation.site).Hostname(), observation.detail)
}

// probeTLSClients holds every TLS client the worker image carries to sites, each client only to the
// sites that hold it (tlsSite.clients). At a fixture chain, a compromised chain is safe only where
// its client refuses it for a certificate error that is no failed signature of a root it holds; at
// a vendored chain, only where it refuses it before the server's signature, which the probe cannot
// make; the control must be accepted; and any other outcome fails the probe. At a site the probe's
// server serves, a verdict counts only where the server began a handshake for the site while the
// client ran, so a client that reached the public host of that name instead, or answered without
// reaching any, fails it. At a public site, an acceptance fails the probe and a refusal is
// recorded; any other outcome is unknown, which fails nothing and is named (tlsUnknownLine), and no
// later client asks that site, so an unreachable host costs one client's bound and no more. An
// optional client the image does not carry is absent at each site, which is recorded and fails
// nothing.
func probeTLSClients(ctx context.Context, clients []tlsClient, sites []tlsSite) ([]tlsObservation, error) {
	var observations []tlsObservation
	var failures []string
	unasked := map[string]string{}
	for _, site := range sites {
		if (site.public || site.vendored) && site.unasked != "" {
			unasked[site.name] = site.unasked
		}
	}
	for _, client := range clients {
		for _, site := range sites {
			if site.clients != nil && !slices.Contains(site.clients, client.name()) || site.vendored && site.unasked != "" {
				continue
			}
			var outcome tlsOutcome
			if reason := unasked[site.name]; site.public && reason != "" {
				outcome = tlsOutcome{status: tlsPublicUnknown, detail: "not asked: " + reason}
			} else {
				outcome = probeTLSSite(ctx, client, site)
			}
			observation := tlsObservation{client: client.name(), site: site, tlsOutcome: outcome}
			switch {
			case outcome.status == tlsClientAbsent || outcome.status == tlsPublicUnknown:
				// Only a tlsOptionalClient is absent, and only where its program is not on PATH.
			case site.public && outcome.status == tlsConnectionAccepted:
				failures = append(failures, fmt.Sprintf("%s accepts the %s chain: %s", client.name(), site.name, outcome.detail))
			case site.public && outcome.status != tlsCertificateRefused && outcome.detail != tlsProbeDeadline:
				// A host the client could not reach, or that gave it no verdict. The probe's own
				// deadline is no answer of the host's, and fails the probe below, closed.
				observation.status = tlsPublicUnknown
				unasked[site.name] = fmt.Sprintf("%s gave no verdict there: %s", client.name(), outcome.detail)
			case site.control && outcome.status != tlsConnectionAccepted:
				failures = append(failures, fmt.Sprintf("network unavailable: %s cannot reach %s: %s", client.name(), site.url, outcome.detail))
			case !site.control && outcome.status == tlsConnectionAccepted:
				failures = append(failures, fmt.Sprintf("%s accepts the %s chain: %s", client.name(), site.name, outcome.detail))
			case site.vendored && outcome.signatureFailed:
				// A store that holds the CA-published root accepts the chain and fails the server's
				// signature; one that holds a root of that name and another key fails that root's. Both
				// trust a root named as the distrusted one.
				failures = append(failures, fmt.Sprintf("%s accepts the %s chain: it trusts a root of that chain's root's name, and got past the chain's names to a signature check, which the probe's chains cannot pass: %s", client.name(), site.name, outcome.detail))
			case !site.control && !site.public && outcome.signatureFailed:
				failures = append(failures, fmt.Sprintf("%s holds the %s root: its refusal failed the signature of a root of that name in its own store: %s", client.name(), site.name, outcome.detail))
			case !site.control && outcome.status != tlsCertificateRefused:
				failures = append(failures, fmt.Sprintf("%s cannot verify refusal of the %s chain: %s", client.name(), site.name, outcome.detail))
			}
			observations = append(observations, observation)
		}
	}
	if len(failures) > 0 {
		return observations, fmt.Errorf("worker TLS probe failed: %s", strings.Join(failures, "; "))
	}
	return observations, nil
}

// tlsNeverReached begins the detail of a verdict a client gave at a site the probe's server serves
// without the server beginning a handshake for that site (probeTLSSite).
const tlsNeverReached = "never reached the probe's server, so the verdict is not the probe's chain's: "

// tlsProbeDeadline is the detail of a client the probe's own deadline stopped: no outcome of the
// site's, so it fails the probe at every site, a public one too.
const tlsProbeDeadline = "probe deadline"

// probeTLSSite is client's outcome at site. At a site the probe's server serves, a verdict the
// client gave without the server beginning a handshake for that site is no verdict of that site's:
// the client reached some other host, or none.
func probeTLSSite(ctx context.Context, client tlsClient, site tlsSite) tlsOutcome {
	if site.seen == nil {
		return client.probe(ctx, site)
	}
	before := site.seen.Load()
	outcome := client.probe(ctx, site)
	if (outcome.status == tlsCertificateRefused || outcome.status == tlsConnectionAccepted) && site.seen.Load() == before {
		return tlsOutcome{status: tlsNetworkUnavailable, detail: tlsNeverReached + outcome.detail}
	}
	return outcome
}

// runTLSProbe is the worker image's TLS probe, `legion probe-image`'s step after the capability
// check. It starts the probe's server in a state directory of its own and holds it to its fixture
// chains (the positive controls, which fail the probe) and to its vendored chains, then holds the
// image's clients, invocation the image's Oh My Pi, under env:
//
//   - every client to the fixture chains and the control;
//   - every client that verifies a chain during the handshake to the vendored chains, which show
//     its refusal of the public sites' own chains with no network;
//   - each client that verifies a chain only after the handshake (tlsClientSpec.chainAfterHandshake),
//     for which no chain the server cannot sign for shows a refusal, to the public sites; and every
//     client to a public site in place of a vendored chain that failed its positive control.
//
// It prints each line to stdout, an UNKNOWN line for each vendored or public site no client is held
// to and for each client a public site gave no verdict for, and stops the server and removes the
// state directory before it returns.
func runTLSProbe(ctx context.Context, invocation string, env []string, stdout io.Writer) error {
	stateDir, removeStateDir, err := newTLSStateDir()
	if err != nil {
		return err
	}
	defer removeStateDir()
	server, err := newTLSServer(stateDir)
	if err != nil {
		return err
	}
	defer server.Close()
	served := server.sites()
	local := slices.DeleteFunc(slices.Clone(served), func(site tlsSite) bool { return site.vendored })
	if err := newTLSPositiveControls(ctx, env, stateDir, local); err != nil {
		return err
	}
	fmt.Fprintln(stdout, tlsPositiveControlsLine(fixtureTLSSites(local)))
	afterHandshake := tlsChainAfterHandshakeClients()
	duringHandshake := slices.DeleteFunc(workerTLSClientNames(), func(name string) bool { return slices.Contains(afterHandshake, name) })
	vendored := newVendoredTLSSites(ctx, env, stateDir, served)
	public := newPublicTLSSites(ctx, env, stateDir)
	for index := range vendored {
		vendored[index].clients = duringHandshake
		fmt.Fprintln(stdout, tlsSiteLine(vendored[index]))
	}
	for index := range public {
		public[index].clients = afterHandshake
		for _, site := range vendored {
			if site.url == public[index].url && site.unasked != "" {
				public[index].clients = nil
			}
		}
		fmt.Fprintln(stdout, tlsSiteLine(public[index]))
	}
	observations, err := probeTLSClients(ctx, newImageTLSClients(invocation, env, stateDir), slices.Concat(local, vendored, public))
	for _, observation := range observations {
		fmt.Fprintf(stdout, "probe-image: TLS client %s %s: %s (%s)\n",
			observation.client, observation.site.name, observation.status, observation.detail)
	}
	for _, observation := range observations {
		if observation.status == tlsPublicUnknown {
			fmt.Fprintln(stdout, tlsUnknownLine(observation))
		}
	}
	return err
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
		return tlsOutcome{status: tlsNetworkUnavailable, detail: tlsProbeDeadline}
	}
	message := tlsCommandMessage(err, output)
	detail := trimTLSDetail(message)
	switch {
	case err == nil || c.applicationSeen != nil && c.applicationSeen(message):
		return tlsOutcome{status: tlsConnectionAccepted, detail: detail}
	case tlsCertificateError(message):
		return tlsOutcome{status: tlsCertificateRefused, detail: detail, signatureFailed: tlsSignatureFailure(message)}
	default:
		return tlsOutcome{status: tlsNetworkUnavailable, detail: detail, signatureFailed: tlsSignatureFailure(message)}
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
		return tlsOutcome{status: tlsNetworkUnavailable, detail: tlsProbeDeadline}
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
// a credential to the server it reached (TestImageTLSClientsCarryNoCredentialOfThePods).
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
// in the state directory, which replaces the image's, the state directory as its working
// directory (tlsClientSpec.build), the settings its command gives, and LEGION_TLS_URL naming the
// site. The state directory holds the probe's access-token file, go.mod and pyproject.toml
// (newTLSStateDir) and what the clients keep there, so no configuration file of the pod's working
// directory is read. Five clients that read configuration from somewhere neither moves are each
// given the flag, variable or file that stops it, with its measurement at the client's spec
// (workerTLSClients): curl's -q (its passwd home's curlrc), mise's --no-config (its working
// directory's parents), the state directory's workspace pyproject.toml for uv (a parent's uv.toml
// or pyproject.toml), git's GIT_CEILING_DIRECTORIES (a parent repository), and go's own go.mod (a
// parent go.mod). mise's --no-config stays although it also skips the image's mise system file,
// /etc/mise/config.toml, which the probe therefore does not read: mise 2026.8.12's `mise settings
// ls --all` names no TLS setting (none for ssl, tls, cert, insecure or proxy), and its netrc
// settings would carry a credential. bun and omp also read configuration from somewhere neither
// moves, the package.json and tsconfig.json of each of their working directory's parents
// (measured on 2026-11-18 with bun 1.4.4 and the omp built on it: a grandparent's of each was
// parsed, and the eval ran), and are given nothing that stops it, since neither file holds a TLS
// setting of Bun's. So no credential variable of the pod's reaches a client, and no file of the
// pod's home or working directory does but one a kept variable names, as SSL_CERT_FILE may.
//
// No proxy variable of the pod's reaches a client. At a site the probe's server serves, each
// client's proxy is that server (tlsProxyEnvironment), so the probe tests the chain that server
// serves whatever the pod's egress. At a public site the probe tests each client's direct path;
// where a pod's roles reach the public sites only through a proxy, their positive controls cannot
// reach them, so no client asks them and each is unknown (probePublicTLSSites), and where a pod
// has a proxy and direct egress both, the probe tests the direct path while the roles' traffic
// takes the proxy, which the probe does not test. The pod's proxy variables are left out because
// a proxy URL can carry a login.
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

// trimTLSDetail is detail, a client's output, as an outcome carries it: what the URL rule leaves
// of the whole of it, and where that is longer than tlsDetailLimit, its first third and its end,
// where a client writes its refusal, with "…" between them. The length is decided on what the
// rule leaves, so the head and the end never overlap. redact.Bytes cuts the head and redact.Tail
// the end, each where a character starts, from the text the rule has read whole: a cut made first
// can stop inside a URL's userinfo or its query, where the rule no longer finds either.
func trimTLSDetail(detail string) string {
	detail = redact.URLCredentials(detail)
	if len(detail) <= tlsDetailLimit {
		return detail
	}
	head, _ := redact.Bytes(detail, tlsDetailLimit/3)
	tail, _ := redact.Tail(detail, tlsDetailLimit-tlsDetailLimit/3-1)
	return head + "…" + tail
}

func tlsCertificateError(detail string) bool {
	lower := strings.ToLower(detail)
	for _, marker := range []string{
		"cert_revoked",
		"certificate revoked",
		"certificate verify failed",
		"invalid peer certificate",
		// git's https transport through libcurl built with GnuTLS words its refusal of a
		// certificate by libcurl's version (curl's lib/vtls/gtls.c): "SSL certificate
		// verification failed: <reason>" from 8.17.0, as git on libcurl 8.18 prints it on Ubuntu
		// 26.04; "server verification failed: <reason>" from 8.10.0 to 8.16.0, as on Debian 13's
		// libcurl 8.14; and "server certificate verification failed" in 8.9.1 and older. This
		// marker reads the first wording alone, and no marker of this list reads the last.
		"ssl certificate verification failed",
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
		// node, CodeGraph's node, bun and omp at the fixture chains, whose roots no store of theirs
		// holds: "Error: unable to get local issuer certificate … code:
		// 'UNABLE_TO_GET_ISSUER_CERT_LOCALLY'" from node and "TypeError: unable to get local issuer
		// certificate … code: \"UNABLE_TO_GET_ISSUER_CERT_LOCALLY\"" from bun, measured on 2026-11-20
		// with node 24.22.1 and bun 1.4.4 and 1.4.5 in the image of 1c7c825367.
		"unable to get local issuer certificate",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// tlsSignatureFailure reports whether detail failed a signature the chain should have carried. At a
// fixture chain it means the client found a root of the fixture root's name and key identifier in
// its own store and failed the fixture's signature: each fixture root has the subject and subject
// key identifier of the CA-published root it stands in for, and a key of its own
// (newTLSFixture), so a store that holds no root of that name refuses the fixture chain
// for an unknown issuer, as it refuses the public site's, and a store that holds the distrusted
// root itself finds it as the issuer and fails the fixture's signature: the store trusts the root
// the probe exists to keep out. At a vendored chain it means the client got past the chain to the
// server's signature, which the probe's server cannot make for a chain whose key it lacks: the
// client accepted the chain. Each marker was measured on 2026-11-20 in the image of 1c7c825367,
// with the CA-published roots added to the store each client reads:
//
//   - at the fixture chains: "certificate signature failure", OpenSSL's verify text, from the AWS
//     CLI, python3, bq, gcloud, az, node, CodeGraph's node, bun and omp, the last four also with
//     the code CERT_SIGNATURE_FAILURE; "BadSignature" from uv's and mise's rustls; Go's "possibly
//     because of … while trying to verify candidate authority certificate" from gh, go and
//     kubectl; and "provider signature failure", the handshake error curl 8.14 and socat gave for
//     the ECDSA emSign root. For the RSA AAA root curl gave "rsa routines::invalid padding", which
//     is no certificate error, so it fails the probe as no verdict, and socat that line before
//     "certificate verify failed", which reads as a refusal; git (GnuTLS) gave "certificate error,
//     no details available" and redis-cli "certificate verify failed" for both, which no marker
//     here reads, and wget and Chromium read no setting that adds a root;
//   - at the vendored chains: OpenSSL's "bad signature" from the AWS CLI, python3, gcloud, bq, az,
//     redis-cli and socat; "provider signature failure" from curl; Go's "invalid signature by the
//     server certificate" from gh and kubectl; and "BadSignature" from mise. go refused the chain
//     itself ("x509: no valid chains built"), and bun, omp and uv refused it as revoked or
//     distrusted, the roots added or not.
func tlsSignatureFailure(detail string) bool {
	lower := strings.ToLower(detail)
	for _, marker := range []string{
		"certificate signature failure",
		"cert_signature_failure",
		"badsignature",
		"while trying to verify candidate authority certificate",
		"provider signature failure",
		"bad signature",
		"invalid signature by the server certificate",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

const tlsFetchProgram = `const response = await fetch(process.env.LEGION_TLS_URL); if (!response.ok) throw new Error("HTTP " + response.status);`

// tlsClientKind is which client tlsClientSpec.build makes.
type tlsClientKind int

const (
	// tlsCommandKind, the zero value, makes a tlsCommandClient.
	tlsCommandKind tlsClientKind = iota
	// tlsOptionalKind makes a tlsOptionalClient, absent where its program is not on PATH.
	tlsOptionalKind
	// tlsChromiumKind makes a tlsChromiumClient, which reads Chromium's navigation result.
	tlsChromiumKind
)

// tlsClientSpec is one client of the TLS probe, which build makes: a client of workerTLSClients,
// or the positive controls' curl (tlsPositiveControlSpec).
type tlsClientSpec struct {
	// name is the client's name in the probe's output.
	name string
	// program is what the client runs, looked up on PATH where it is no path.
	program string
	timeout time.Duration
	// kind is the client build makes, a tlsCommandClient unless it names another.
	kind tlsClientKind
	// readStdout and applicationSeen are the command client's.
	readStdout      bool
	applicationSeen func(string) bool
	// chainAfterHandshake marks a client that verifies the server's chain only once the handshake
	// has completed, so at a vendored chain, which the probe's server cannot sign for, it fails the
	// server's signature before it ever reads the chain, whatever its store holds: node and
	// CodeGraph's node (Node reads OpenSSL's verify result after the handshake and raises it then),
	// and wget and git (GnuTLS, through wget's own TLS code and libcurl's, verifies the peer after
	// gnutls_handshake). Measured on 2026-11-20 in the image of 1c7c825367: at the vendored chains,
	// node gave "provider signature failure" and wget and git "Public key signature verification
	// has failed", with the CA-published roots in their stores and without. Such a client is held to
	// the public sites (runTLSProbe).
	chainAfterHandshake bool
	// command gives one site's arguments and the variables the client sets, each NAME=value, with
	// the client's state under stateDir (newTLSStateDir); invocation is the image's Oh My Pi. At
	// the control it gives the client the site's trust file (tlsSite.trust) in the client's own
	// way, and at a site the probe's server serves, a client with no proxy (redis-cli, socat) or
	// with a proxy setting of its own (Chromium) names the server's address (tlsSite.via) itself.
	command func(site tlsSite, stateDir, invocation string) (args, set []string)
}

// build makes the client spec names, under env, with stateDir as its working directory and a HOME
// of its own, <stateDir>/<name>, then, at a site the probe's server serves, that server as its
// proxy (tlsProxyEnvironment), each given before the variables its command sets, so a client's own
// value replaces it.
func (spec tlsClientSpec) build(invocation string, env []string, stateDir string) tlsClient {
	home := "HOME=" + filepath.Join(stateDir, spec.name)
	client := tlsCommandClient{
		clientName: spec.name, env: env, workDir: stateDir, timeout: spec.timeout,
		applicationSeen: spec.applicationSeen, readStdout: spec.readStdout,
		command: func(site tlsSite) (string, []string, []string) {
			args, set := spec.command(site, stateDir, invocation)
			return spec.program, args, slices.Concat([]string{home}, tlsProxyEnvironment(site), set)
		},
	}
	switch spec.kind {
	case tlsOptionalKind:
		return tlsOptionalClient{tlsCommandClient: client, program: spec.program}
	case tlsChromiumKind:
		return tlsChromiumClient{tlsCommandClient: client}
	default:
		return client
	}
}

// tlsProxyEnvironment is the probe's server as the HTTP and HTTPS proxy of a client at a site that
// server serves, and nothing at a public site: both spellings, since some clients read the
// lower-case ones (curl, git's libcurl, wget) and some the upper-case ones, and node's
// NODE_USE_ENV_PROXY=1, without which node and CodeGraph's node read neither (measured on
// 2026-11-20 with node 24.22.1: without it, a fetch under HTTPS_PROXY resolved the site's name;
// with it, the fetch went to the proxy). Measured on 2026-11-20 in the image of 1c7c825367 with no
// route but loopback, every client of workerTLSClients that reads them reached the server at
// every site it serves, which tlsSite.seen proves at each probe. So no client resolves a site's
// name: the server takes its CONNECT and serves the site's chain, and refuses any other host, so
// nothing else a client fetches (an issuer certificate, a version check) reaches the network.
func tlsProxyEnvironment(site tlsSite) []string {
	if site.via == "" {
		return nil
	}
	proxy := "http://" + site.via
	return []string{"HTTPS_PROXY=" + proxy, "https_proxy=" + proxy, "HTTP_PROXY=" + proxy, "http_proxy=" + proxy, "NODE_USE_ENV_PROXY=1"}
}

// tlsTrustSetting is NAME=<the site's trust file>, the one variable a client is given its trust in
// at the control, and nothing at a site with no trust file.
func tlsTrustSetting(site tlsSite, name string) []string {
	if site.trust == "" {
		return nil
	}
	return []string{name + "=" + site.trust}
}

// tlsTrustArgs is the arguments a client is given its trust in at the control, the trust file
// joined to flag ("--cacert" and the file as two, "--ca-certificate=" and the file as one), and
// nothing at a site with no trust file.
func tlsTrustArgs(site tlsSite, flag string) []string {
	if site.trust == "" {
		return nil
	}
	if strings.HasSuffix(flag, "=") {
		return []string{flag + site.trust}
	}
	return []string{flag, site.trust}
}

// tlsProbeSignIn is the dummy credential gcloud, bq and kubectl sign in with: gcloud and bq from
// the state directory's tlsAccessTokenFile and, for bq, its --oauth_access_token flag, and kubectl
// from its --token flag. The probe needs their TLS handshake, never an authorized call. Its name
// holds no "token", so the public leak check does not read the literal as a credential.
const (
	tlsProbeSignIn     = "legion-tls-probe"
	tlsAccessTokenFile = "access-token"
)

// newTLSStateDir makes the TLS probe's state directory: every client's working directory and the
// directory its HOME is under (tlsClientSpec.build), where gcloud, bq, az, mise, go and kubectl
// keep their state too, so none of it lands in the runtime user's home (the image build's final
// step requires that). It is made holding three files: the access-token file gcloud and bq sign
// in with; a go.mod, which go reads in place of a parent directory's; and a pyproject.toml
// holding only [tool.uv.workspace], which makes the state directory uv's workspace root (the go
// and uv clients of workerTLSClients say why). Its path is made absolute, and one holding a colon
// refused, because git skips a relative GIT_CEILING_DIRECTORIES entry and splits the value at
// colons, either of which would turn git's ceiling off. remove deletes the directory with
// everything the clients wrote there.
func newTLSStateDir() (dir string, remove func(), err error) {
	made, err := os.MkdirTemp("", "legion-tls-probe-")
	if err != nil {
		return "", nil, fmt.Errorf("make the TLS probe's state directory: %w", err)
	}
	if dir, err = filepath.Abs(made); err != nil {
		_ = os.RemoveAll(made)
		return "", nil, fmt.Errorf("make the TLS probe's state directory %s absolute: %w", made, err)
	}
	remove = func() { _ = os.RemoveAll(dir) }
	if strings.Contains(dir, ":") {
		remove()
		return "", nil, fmt.Errorf("the TLS probe's state directory %s holds a colon, which git reads as a separator in GIT_CEILING_DIRECTORIES", dir)
	}
	if err = os.WriteFile(filepath.Join(dir, tlsAccessTokenFile), []byte(tlsProbeSignIn), 0o600); err != nil {
		remove()
		return "", nil, fmt.Errorf("write the TLS probe's access token: %w", err)
	}
	if err = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module legion-tls-probe\n"), 0o600); err != nil {
		remove()
		return "", nil, fmt.Errorf("write the TLS probe's go.mod: %w", err)
	}
	if err = os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[tool.uv.workspace]\n"), 0o600); err != nil {
		remove()
		return "", nil, fmt.Errorf("write the TLS probe's pyproject.toml: %w", err)
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
// No client carries a credential of the pod's to a site, and none reads a file of the pod's home
// or working directory but one a kept variable names: tlsKeptVariables says how.
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
		name: "node", program: "node", timeout: tlsDefaultProbeTimeout, chainAfterHandshake: true,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			return []string{"--input-type=module", "--eval", tlsFetchProgram}, tlsTrustSetting(site, "NODE_EXTRA_CA_CERTS")
		},
	},
	{
		name: "bun", program: "bun", timeout: tlsDefaultProbeTimeout,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			return []string{"--eval", tlsFetchProgram}, tlsTrustSetting(site, "NODE_EXTRA_CA_CERTS")
		},
	},
	{
		name: "omp", program: "sh", timeout: tlsDefaultProbeTimeout,
		command: func(site tlsSite, _, invocation string) ([]string, []string) {
			return []string{"-c", "exec " + invocation + " --eval \"$1\"", "sh", tlsFetchProgram}, append([]string{"BUN_BE_BUN=1"}, tlsTrustSetting(site, "NODE_EXTRA_CA_CERTS")...)
		},
	},
	{
		name: "codegraph-node", program: "/opt/codegraph/node", timeout: tlsDefaultProbeTimeout, chainAfterHandshake: true,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			return []string{"--input-type=module", "--eval", tlsFetchProgram}, tlsTrustSetting(site, "NODE_EXTRA_CA_CERTS")
		},
	},
	// uv reads its settings from its workspace root, and the state directory's pyproject.toml,
	// which declares only a workspace (newTLSStateDir), makes the state directory that root, so uv
	// reads no parent's pyproject.toml or uv.toml; it still reads the image's system uv.toml
	// (/etc/uv/uv.toml, or uv/uv.toml under XDG_CONFIG_DIRS) and merges it under that file, which
	// overrides none of its settings, and it asks an index that system file names before the site,
	// whose answer the probe may then read as the site's (uvResponseSeen). --no-config would skip
	// the system file too. Measured on 2026-11-18, uv 0.13.5, with a parent's pyproject.toml
	// declaring a project and offline = true: with an empty uv.toml in place of this file, uv took
	// the parent as its workspace root and resolved offline; with this file, it took the state
	// directory and failed at the fetch, reading neither that pyproject.toml nor a broken uv.toml
	// added beside it; and it read a system uv.toml given through XDG_CONFIG_DIRS.
	{
		name: "uv", program: "uv", timeout: tlsDefaultProbeTimeout, applicationSeen: uvResponseSeen,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			var trust []string
			if site.trust != "" {
				trust = []string{"UV_SYSTEM_CERTS=true", "SSL_CERT_FILE=" + site.trust}
			}
			return []string{"pip", "install", "--system", "--break-system-packages", "--dry-run", "--no-deps", "--no-cache", "--index-url", site.url, "legion-tls-probe-not-a-package"}, trust
		},
	},
	{
		name: "aws", program: "aws", timeout: tlsDefaultProbeTimeout, applicationSeen: awsResponseSeen,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			return append([]string{"sts", "get-caller-identity", "--no-sign-request", "--region", "us-east-1", "--endpoint-url", site.url}, tlsTrustArgs(site, "--ca-bundle")...), nil
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
			return slices.Concat([]string{"-q"}, tlsTrustArgs(site, "--cacert"), []string{"--fail", "--silent", "--show-error", site.url}), nil
		},
	},
	{
		name: "python3", program: "python3", timeout: tlsDefaultProbeTimeout,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			return []string{"-c", "import os, urllib.request; urllib.request.urlopen(os.environ['LEGION_TLS_URL']).read(1)"}, tlsTrustSetting(site, "SSL_CERT_FILE")
		},
	},
	{
		name: "gh", program: "/usr/local/bin/gh", timeout: tlsDefaultProbeTimeout, applicationSeen: ghResponseSeen,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			return []string{"api", site.url}, append([]string{"GH_TOKEN=" + tlsProbeSignIn}, tlsTrustSetting(site, "SSL_CERT_FILE")...)
		},
	},
	// --no-hsts keeps wget from writing ~/.wget-hsts into the runtime user's home. wget exits 0 on
	// the control's page, so, as curl, it needs no response marker.
	{
		name: "wget", program: "wget", timeout: tlsDefaultProbeTimeout, chainAfterHandshake: true,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			return slices.Concat([]string{"--no-verbose", "--no-hsts", "--tries=1", "--output-document=/dev/null"}, tlsTrustArgs(site, "--ca-certificate="), []string{site.url}), nil
		},
	},
	// The PATH's git: in a pod, the /opt/legion/bin/git wrapper, which hands ls-remote to
	// /usr/bin/git unchanged. GIT_TERMINAL_PROMPT=0 keeps it from waiting on a credential prompt.
	// Its user configuration, whose http.extraHeader git sends to every host, is under its own empty
	// HOME, and no GIT_CONFIG_* variable of the pod's reaches it (tlsKeptVariables).
	// GIT_CEILING_DIRECTORIES at the state directory's parent keeps it from a parent repository's
	// .git/config (measured on 2026-11-18, git 2.47: from a repository's child directory, `git
	// config --get http.extraheader` printed that repository's header, and printed none with the
	// ceiling at the child's parent; a ceiling at the child itself did not stop it). So git reads
	// the image's system configuration, /etc/gitconfig, and no other.
	// No site is a git repository, so git fails ls-remote at the control too, after the handshake;
	// gitSiteAnswer reads that answer.
	{
		name: "git", program: "git", timeout: tlsDefaultProbeTimeout, applicationSeen: gitSiteAnswer.MatchString, chainAfterHandshake: true,
		command: func(site tlsSite, stateDir, _ string) ([]string, []string) {
			return []string{"ls-remote", site.url}, append([]string{"GIT_TERMINAL_PROMPT=0", "GIT_CEILING_DIRECTORIES=" + filepath.Dir(stateDir)}, tlsTrustSetting(site, "GIT_SSL_CAINFO")...)
		},
	},
	// go's fetch of a module path at the site's host, the fetch `go install` and `go get` make with
	// GOPROXY=direct. go reads the first go.mod it finds from its working directory up to /, and
	// nothing turns that walk off, so the state directory holds a go.mod of its own
	// (newTLSStateDir), which go reads in place of a parent's (measured on 2026-11-18, go 1.27.3: a
	// parent's go.mod needing go 1.99 failed it with "go: ../go.mod requires go >= 1.99", and with
	// the state directory's go.mod go read that one, and its answer at the emSign site was the line
	// it gave with no go.mod at all). The variables below keep every path go writes under the state
	// directory. GOTOOLCHAIN=local keeps it from downloading a toolchain, and GOAUTH=off from
	// running any authentication command. No site serves a Go module path, so go fails at the
	// control too; goGetAnswer reads the site's answer.
	{
		name: "go", program: "go", timeout: tlsDefaultProbeTimeout, applicationSeen: goGetAnswer.MatchString,
		command: func(site tlsSite, stateDir, _ string) ([]string, []string) {
			home := filepath.Join(stateDir, "go")
			return []string{"mod", "download", tlsSiteURL(site).Hostname() + "/legion-tls-probe@latest"}, append([]string{
				"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
				"GOPATH=" + filepath.Join(home, "path"),
				"GOMODCACHE=" + filepath.Join(home, "mod"),
				"GOCACHE=" + filepath.Join(home, "cache"),
				"GOPROXY=direct",
				"GOTOOLCHAIN=local",
				"GOFLAGS=",
				"GOWORK=off",
				"GOAUTH=off",
			}, tlsTrustSetting(site, "SSL_CERT_FILE")...)
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
			return slices.Concat([]string{"--server=" + site.url, "--token=" + tlsProbeSignIn}, tlsTrustArgs(site, "--certificate-authority="), []string{"get", "--raw", "/"}), nil
		},
	},
	// redis-cli's TLS connection to the site's host and port, with the host as its SNI name, the
	// way `redis-cli --tls -h <host> -p <port>` reaches a Redis server a role names; at a site the
	// probe's server serves, redis-cli, which has no proxy, connects to that server's address and
	// names the site's host in its SNI alone. No site speaks Redis, so redis-cli fails at the
	// control too, past the handshake; redisResponseSeen reads that answer. redis-cli sends
	// REDISCLI_AUTH as AUTH to the server it reaches, here a test site (measured on 2026-11-18 at
	// example.com with a dummy value set, redis-cli 8.0.2 printed "I/O error" for the AUTH before
	// its protocol error), and no kept variable is REDISCLI_AUTH, so the pod's never reaches it.
	{
		name: "redis-cli", program: "redis-cli", timeout: tlsDefaultProbeTimeout, applicationSeen: redisResponseSeen,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			sni, address, port := tlsSiteAddress(site)
			return slices.Concat([]string{"--tls", "--sni", sni, "-h", address, "-p", port}, tlsTrustArgs(site, "--cacert"), []string{"PING"}), nil
		},
	},
	// gcloud's compute call, with the compute endpoint at the site. `gcloud storage` is not the
	// form because it retried for 35-41 s before it answered. At the control it is given the
	// control's root in core/custom_ca_certs_file and in the two variables requests reads, which a
	// pod may set: measured on 2026-11-20 with gcloud 594.0.0, under a pod's REQUESTS_CA_BUNDLE and
	// CURL_CA_BUNDLE naming a bundle without the control's root, gcloud refused the control whose
	// root the property alone named.
	{
		name: "gcloud", program: "gcloud", timeout: tlsDefaultProbeTimeout, kind: tlsOptionalKind, applicationSeen: gcloudResponseSeen,
		command: func(site tlsSite, stateDir, _ string) ([]string, []string) {
			return []string{"compute", "regions", "list", "--project", "legion-tls-probe"},
				slices.Concat(googleCloudTLSEnvironment(stateDir, "gcloud"), []string{"CLOUDSDK_API_ENDPOINT_OVERRIDES_COMPUTE=" + site.url + "compute/v1/"},
					tlsTrustSetting(site, "CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE"), tlsTrustSetting(site, "REQUESTS_CA_BUNDLE"), tlsTrustSetting(site, "CURL_CA_BUNDLE"))
		},
	},
	// --oauth_access_token makes the SDK's bq launcher skip its own credential lookup, which asks
	// the metadata server (bin/bootstrapping/bq.py) and, with no account, refuses with "You do not
	// currently have an active account selected"; the token file still serves the `gcloud config`
	// subprocess bq runs. bq writes its error to stdout, so the client reads stdout too.
	{
		name: "bq", program: "bq", timeout: tlsBigQueryProbeTimeout, kind: tlsOptionalKind, readStdout: true, applicationSeen: bqResponseSeen,
		command: func(site tlsSite, stateDir, _ string) ([]string, []string) {
			return []string{"--oauth_access_token=" + tlsProbeSignIn, "--api=" + site.url, "ls", "--project_id=legion-tls-probe"},
				append(googleCloudTLSEnvironment(stateDir, "bq"), tlsTrustSetting(site, "CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE")...)
		},
	},
	// One configuration directory for az's every site, so the version fetch from
	// azcliprod.blob.core.windows.net, which az makes at its first command in a configuration
	// directory, happens once a probe. az exited 0 on the control's page, so, as curl, it needs no
	// response marker.
	{
		name: "az", program: "az", timeout: tlsDefaultProbeTimeout, kind: tlsOptionalKind,
		command: func(site tlsSite, stateDir, _ string) ([]string, []string) {
			return []string{"rest", "--method", "get", "--url", site.url, "--skip-authorization-header"},
				append([]string{"AZURE_CONFIG_DIR=" + filepath.Join(stateDir, "az", "config")}, tlsTrustSetting(site, "REQUESTS_CA_BUNDLE")...)
		},
	},
	// mise's http: backend, downloading the site as a tool, in a directory of its own per site so
	// one site's download never satisfies the next. MISE_VERBOSE prints the status line
	// miseHTTPStatus reads. --no-config, first, keeps mise from every config file, where it reads a
	// mise.toml of each directory from its working directory up to / (measured on 2026-11-18, mise
	// 2026.8.12: a parent's mise.toml was read, and with --no-config neither it, the working
	// directory's .mise.toml nor a config.toml in MISE_CONFIG_DIR was). It also skips the image's
	// system file, /etc/mise/config.toml or the one MISE_SYSTEM_CONFIG_FILE names, which the probe
	// therefore does not read (tlsKeptVariables): measured on 2026-11-18 with mise 2026.8.12, with
	// MISE_SYSTEM_CONFIG_FILE naming a broken file, mise failed on it without --no-config and did
	// not read it with it.
	{
		name: "mise", program: "mise", timeout: tlsDefaultProbeTimeout, kind: tlsOptionalKind, applicationSeen: miseHTTPStatus.MatchString,
		command: func(site tlsSite, stateDir, _ string) ([]string, []string) {
			home := filepath.Join(stateDir, "mise", strings.ReplaceAll(site.name, " ", "-"))
			return []string{"--no-config", "x", "http:legion-tls-probe[url=" + site.url + "]@1", "--", "true"}, append([]string{
				"HOME=" + home,
				"MISE_DATA_DIR=" + filepath.Join(home, "data"),
				"MISE_CACHE_DIR=" + filepath.Join(home, "cache"),
				"MISE_CONFIG_DIR=" + filepath.Join(home, "config"),
				"MISE_STATE_DIR=" + filepath.Join(home, "state"),
				"MISE_YES=1",
				"MISE_VERBOSE=1",
			}, tlsTrustSetting(site, "SSL_CERT_FILE")...)
		},
	},
	// socat's OPENSSL: address at the site's host and port, which socat verifies the leaf's name
	// against; at a site the probe's server serves, socat, which has no proxy, connects to that
	// server's address and names the site's host as the name to verify and in its SNI. Its stdin
	// is the probe's /dev/null, so it completes the handshake, sends nothing and exits. It exits 0
	// on the control, so, as curl, it needs no response marker, and it keeps no state.
	{
		name: "socat", program: "socat", timeout: tlsDefaultProbeTimeout, kind: tlsOptionalKind,
		command: func(site tlsSite, _, _ string) ([]string, []string) {
			sni, address, port := tlsSiteAddress(site)
			target := "OPENSSL:" + address + ":" + port
			if site.via != "" {
				target += ",commonname=" + sni + ",snihost=" + sni
			}
			if site.trust != "" {
				target += ",cafile=" + site.trust
			}
			return []string{"-u", "-", target}, nil
		},
	},
	{
		name: "chromium", program: "chromium", timeout: tlsChromiumProbeTimeout, kind: tlsChromiumKind,
		command: func(site tlsSite, stateDir, _ string) ([]string, []string) {
			return chromiumTLSArgs(site, stateDir), nil
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
// ERR_NAME_NOT_RESOLVED" (net/dns/mapped_host_resolver.h).
//
// At a site the probe's server serves, Chromium takes that server as its proxy (--proxy-server),
// which it reaches by address, so the rule excludes that address alone and Chromium resolves no
// name at all; the fixture chains carry no AIA URL. At the control, Chromium is given the control's
// root as --ignore-certificate-errors-spki-list, the SHA-256 of the root's public key, which the
// control's chain carries (newTLSControlFixture): Chromium on Linux keeps the roots a user adds
// in an NSS database, and the image carries no certutil to write one. Chromium reads that switch
// only beside --user-data-dir, so the control's Chromium runs with a profile of its own in the
// state directory.
func chromiumTLSArgs(site tlsSite, stateDir string) []string {
	args := []string{"--headless", "--no-sandbox"}
	if site.via == "" {
		args = append(args, "--host-resolver-rules=MAP * ^NOTFOUND, EXCLUDE "+tlsSiteURL(site).Hostname())
	} else {
		host, _, _ := net.SplitHostPort(site.via)
		args = append(args, "--proxy-server=http://"+site.via, "--host-resolver-rules=MAP * ^NOTFOUND, EXCLUDE "+host)
	}
	if site.trust != "" {
		args = append(args, "--user-data-dir="+filepath.Join(stateDir, "chromium-control"), "--ignore-certificate-errors-spki-list="+tlsSPKIHash(site.trust))
	}
	return append(args, "--dump-dom", site.url)
}

// tlsSPKIHash is the base64 SHA-256 of the public key of the first certificate in the PEM file at
// path, the form --ignore-certificate-errors-spki-list takes, or "" where the file holds none: then
// Chromium is given no key, and refuses the control, which fails the probe.
func tlsSPKIHash(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return ""
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// tlsSiteURL is site's URL, parsed. Every site's URL is an absolute https URL, which url.Parse
// reads without error.
func tlsSiteURL(site tlsSite) *url.URL {
	parsed, _ := url.Parse(site.url)
	return parsed
}

// tlsSiteAddress is the name a client verifies and sends in its SNI at site, the site's host, and
// the address and port it connects to: the site's host and port 443, as no site's URL names
// another, or, at a site the probe's server serves, that server's address and port.
func tlsSiteAddress(site tlsSite) (sni, address, port string) {
	sni = tlsSiteURL(site).Hostname()
	if site.via != "" {
		address, port, _ = net.SplitHostPort(site.via)
		return sni, address, port
	}
	return sni, sni, "443"
}

// imageTLSClients returns the image's TLS-capable programs. The gh wrapper manages worker tokens,
// so the probe runs the real gh binary it wraps; no probe uses an operator token. stateDir is the
// probe's state directory (newTLSStateDir).
func imageTLSClients(invocation string, env []string, stateDir string) []tlsClient {
	clients := make([]tlsClient, 0, len(workerTLSClients))
	for _, spec := range workerTLSClients {
		clients = append(clients, spec.build(invocation, env, stateDir))
	}
	return clients
}

// tlsChainAfterHandshakeClients names the clients of workerTLSClients that verify a chain only
// after the handshake (tlsClientSpec.chainAfterHandshake), in the inventory's order.
func tlsChainAfterHandshakeClients() []string {
	var names []string
	for _, spec := range workerTLSClients {
		if spec.chainAfterHandshake {
			names = append(names, spec.name)
		}
	}
	return names
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

// uvResponseSeen reads an answer uv got from the site: an index without the package ("No solution
// found when resolving dependencies", the control), or a page that is no package index, which uv
// names "Unsupported `Content-Type`" (the emSign and AAA sites' text/plain page, measured on
// 2026-11-18 with uv 0.13.5 on the image of 841974e9 in `legion probe-image`, with an
// /etc/uv/uv.toml allowing both hosts and with UV_INSECURE_HOST naming them). Reading both keeps a
// uv that accepted a compromised chain reported as accepting it rather than as unavailable.
// Either answer can instead come from an index the image's system uv.toml names (extra-index-url,
// [[index]]), which uv asks before the --index-url site: measured on 2026-11-18 with uv 0.13.5,
// such an index answering 401 or 403 made uv print "No solution found when resolving
// dependencies", one answering a 200 text/plain page made it print "Unsupported `Content-Type`"
// naming that index's URL, and in both uv never contacted the site. The probe then reads uv as
// accepting the compromised chain of a site it never reached: it still fails, but names that
// index's answer as uv accepting the chain.
var uvResponseSeen = tlsAnyMarker("No solution found when resolving dependencies", "Unsupported `Content-Type`")

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
