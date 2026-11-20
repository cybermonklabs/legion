package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

type fakeTLSClient struct {
	clientName string
	outcomes   map[string]tlsOutcome
}

func (c fakeTLSClient) name() string { return c.clientName }

func (c fakeTLSClient) probe(_ context.Context, site tlsSite, _ tlsRunTerms) tlsOutcome {
	return c.outcomes[site.name]
}

func passingImageTLSClients(string, []string, string) []tlsClient {
	outcomes := map[string]tlsOutcome{
		emSignTLSSite.name:  {status: tlsCertificateRefused, detail: "CERT_REVOKED"},
		aaaTLSSite.name:     {status: tlsCertificateRefused, detail: "CERT_REVOKED"},
		controlTLSSite.name: {status: tlsConnectionAccepted, detail: "HTTP 200"},
	}
	names := workerTLSClientNames()
	clients := make([]tlsClient, 0, len(names))
	for _, name := range names {
		clients = append(clients, fakeTLSClient{clientName: name, outcomes: outcomes})
	}
	return clients
}

func TestProbeTLSClientsDecisions(t *testing.T) {
	refused := tlsOutcome{status: tlsCertificateRefused, detail: "CERT_REVOKED"}
	accepted := tlsOutcome{status: tlsConnectionAccepted, detail: "HTTP 200"}
	unavailable := tlsOutcome{status: tlsNetworkUnavailable, detail: "dial tcp: network is unreachable"}

	for _, testCase := range []struct {
		name     string
		outcomes map[string]tlsOutcome
		want     string
	}{
		{
			name: "both compromised chains refused and control reachable",
			outcomes: map[string]tlsOutcome{
				emSignTLSSite.name:  refused,
				aaaTLSSite.name:     refused,
				controlTLSSite.name: accepted,
			},
		},
		{
			name: "compromised chain accepted",
			outcomes: map[string]tlsOutcome{
				emSignTLSSite.name:  accepted,
				aaaTLSSite.name:     refused,
				controlTLSSite.name: accepted,
			},
			want: "node accepts the emSign chain",
		},
		{
			name: "compromised chain neither refused nor accepted",
			outcomes: map[string]tlsOutcome{
				emSignTLSSite.name:  unavailable,
				aaaTLSSite.name:     refused,
				controlTLSSite.name: accepted,
			},
			want: "node cannot verify refusal of the emSign chain: dial tcp: network is unreachable",
		},
		{
			name: "control unreachable",
			outcomes: map[string]tlsOutcome{
				emSignTLSSite.name:  refused,
				aaaTLSSite.name:     refused,
				controlTLSSite.name: unavailable,
			},
			want: "network unavailable: node cannot reach https://example.com/: dial tcp: network is unreachable",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := probeTLSClients(context.Background(), []tlsClient{fakeTLSClient{clientName: "node", outcomes: testCase.outcomes}}, compromisedTLSSites())
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("probeTLSClients() = %v, want success", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("probeTLSClients() = %v, want error containing %q", err, testCase.want)
			}
		})
	}
}

func TestTLSCertificateErrorRecognizesClientRefusals(t *testing.T) {
	for _, detail := range []string{
		"Error: certificate revoked",
		"invalid peer certificate: Other(OtherError(CertDistrusted))",
		"SSL validation failed: certificate verify failed",
		"curl: (60) SSL certificate problem",
		"x509: certificate signed by unknown authority",
	} {
		if !tlsCertificateError(detail) {
			t.Errorf("tlsCertificateError(%q) = false, want true", detail)
		}
	}
}

// curl names one refusal in several ways, by version and by TLS backend, and an image may carry
// any of them: an image built on another distribution keeps that distribution's curl. git 2.53.0's
// https transport, through libcurl's GnuTLS build, printed the Ubuntu row in the image probe of a
// worker image built on Ubuntu 26.04 on 2026-11-19. Read as no certificate error, a refusal is
// "unavailable" and the probe fails an image that is safe.
func TestTLSCertificateErrorRecognizesEachCurlRefusal(t *testing.T) {
	for _, testCase := range []struct {
		name, detail string
		want         bool
	}{
		{"curl 8.14", "curl: (60) SSL certificate problem: unable to get local issuer certificate", true},
		{"curl 8.18", "curl: (60) SSL certificate OpenSSL verify result: unable to get local issuer certificate (20)", true},
		{"git 2.53.0 on Ubuntu 26.04", `fatal: unable to access 'https://dv-g3b-emudhra.we-have-a-problem.com/': SSL certificate verification failed: certificate signer not trusted. (CAfile: /etc/ssl/certs/ca-certificates.crt CRLfile: none)`, true},
		{"no route", "curl: (7) Failed to connect to example.com port 443 after 3 ms: Could not connect to server", false},
		{"no such host", `fatal: unable to access 'https://dv-g3b-emudhra.we-have-a-problem.com/': Could not resolve host: dv-g3b-emudhra.we-have-a-problem.com`, false},
		{"timeout", "curl: (28) SSL connection timeout", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := tlsCertificateError(testCase.detail); got != testCase.want {
				t.Fatalf("tlsCertificateError(%q) = %v, want %v", testCase.detail, got, testCase.want)
			}
		})
	}
}

// What the floor clients wget, git, go and kubectl wrote to stderr at the emSign site and at the
// control, with the forms workerTLSClients runs, in an INFRA issue pod on 2026-11-18 (GNU wget
// 1.25.0, git 2.47.3, go 1.27.3, kubectl 1.35.9), each output's lines joined by a space as the
// probe's detail joins them. kubectl wrote the control's page to stdout, which the probe discards.
const (
	wgetEmSignRefusal    = `ERROR: The certificate of 'dv-g3b-emudhra.we-have-a-problem.com' is not trusted. ERROR: The certificate of 'dv-g3b-emudhra.we-have-a-problem.com' doesn't have a known issuer.`
	wgetControlAnswer    = `2026-11-18 06:11:03 URL:https://example.com/ [577] -> "/dev/null" [1]`
	gitEmSignRefusal     = `fatal: unable to access 'https://dv-g3b-emudhra.we-have-a-problem.com/': server verification failed: certificate signer not trusted. (CAfile: /etc/ssl/certs/ca-certificates.crt CRLfile: none)`
	gitControlAnswer     = `fatal: repository 'https://example.com/' not found`
	goEmSignRefusal      = `go: dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe@latest: unrecognized import path "dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe": https fetch: Get "https://dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe?go-get=1": tls: failed to verify certificate: x509: certificate signed by unknown authority`
	goControlAnswer      = `go: example.com/legion-tls-probe@latest: unrecognized import path "example.com/legion-tls-probe": reading https://example.com/legion-tls-probe?go-get=1: 404 Not Found`
	kubectlEmSignRefusal = `Unable to connect to the server: tls: failed to verify certificate: x509: certificate signed by unknown authority`
	// git and go with their certificate checks turned off (GIT_SSL_NO_VERIFY=true, GOINSECURE=*),
	// at the emSign site, measured in the same pod: what each says once it has accepted a
	// compromised chain.
	gitEmSignAccepted = `fatal: https://dv-g3b-emudhra.we-have-a-problem.com/info/refs not valid: is this a git repository?`
	goEmSignAccepted  = `go: dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe@latest: unrecognized import path "dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe": parse https://dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe?go-get=1: no go-import meta tags ()`
	// redis-cli 8.0.2's, measured on 2026-11-18 on the pull request's image (sha256:25d2edfc…) as
	// `redis-cli --tls --sni <host> -h <host> -p 443 PING`: the emSign site refuses at the
	// handshake, example.com answers past it with HTTP, and with --insecure the emSign site closes
	// the connection only after the handshake.
	redisEmSignRefusal  = `Could not connect to Redis at dv-g3b-emudhra.we-have-a-problem.com:443: SSL_connect failed: certificate verify failed`
	redisControlAnswer  = `Error: Protocol error, got "H" as reply type byte`
	redisEmSignAccepted = `Error: Server closed the connection`
)

func TestTLSCommandDetailKeepsCertificateErrorTail(t *testing.T) {
	detail := trimTLSDetail(tlsCommandMessage(nil, strings.Repeat("x", tlsDetailLimit)+" invalid peer certificate"))
	if !tlsCertificateError(detail) {
		t.Fatalf("detail = %q, dropped the certificate refusal", detail)
	}
}

func TestTLSCommandClientClassifiesApplicationErrorBeforeTruncatingDetail(t *testing.T) {
	response := strings.Repeat("x", tlsDetailLimit) + " invalid XML received " + strings.Repeat("y", tlsDetailLimit)
	client := tlsCommandClient{
		clientName:      "aws",
		timeout:         tlsDefaultProbeTimeout,
		applicationSeen: awsResponseSeen,
		command: func(tlsSite) (string, []string, []string) {
			return "sh", []string{"-c", `printf %s "$1" >&2; exit 255`, "sh", response}, nil
		},
	}

	outcome := client.probe(context.Background(), controlTLSSite, tlsRunTerms{})

	if outcome.status != tlsConnectionAccepted {
		t.Fatalf("client.probe() = %+v, want an accepted application response", outcome)
	}
}

func TestImageTLSClientsCoverTheWorkerInventory(t *testing.T) {
	clients := imageTLSClients("omp", nil, "")
	want := []struct {
		name     string
		timeout  time.Duration
		optional bool
	}{
		{"node", tlsDefaultProbeTimeout, false},
		{"bun", tlsDefaultProbeTimeout, false},
		{"omp", tlsDefaultProbeTimeout, false},
		{"codegraph-node", tlsDefaultProbeTimeout, false},
		{"uv", tlsDefaultProbeTimeout, false},
		{"aws", tlsDefaultProbeTimeout, false},
		{"curl", tlsDefaultProbeTimeout, false},
		{"python3", tlsDefaultProbeTimeout, false},
		{"gh", tlsDefaultProbeTimeout, false},
		{"wget", tlsDefaultProbeTimeout, false},
		{"git", tlsDefaultProbeTimeout, false},
		{"go", tlsDefaultProbeTimeout, false},
		{"kubectl", tlsDefaultProbeTimeout, false},
		{"redis-cli", tlsDefaultProbeTimeout, false},
		{"gcloud", tlsDefaultProbeTimeout, true},
		{"bq", tlsBigQueryProbeTimeout, true},
		{"az", tlsDefaultProbeTimeout, true},
		{"mise", tlsDefaultProbeTimeout, true},
		{"socat", tlsDefaultProbeTimeout, true},
		{"chromium", tlsChromiumProbeTimeout, false},
	}
	if len(workerTLSClients) != len(want) || len(clients) != len(want) {
		t.Fatalf("inventory has %d specifications and %d clients, want %d", len(workerTLSClients), len(clients), len(want))
	}
	for index, expected := range want {
		spec := workerTLSClients[index]
		if spec.name != expected.name || spec.timeout != expected.timeout || clients[index].name() != expected.name {
			t.Errorf("inventory client %d = (%q, %s, %q), want (%q, %s, %q)", index, spec.name, spec.timeout, clients[index].name(), expected.name, expected.timeout, expected.name)
		}
		var timeout time.Duration
		optional := false
		switch client := clients[index].(type) {
		case tlsCommandClient:
			timeout = client.timeout
		case tlsOptionalClient:
			timeout, optional = client.timeout, true
		case tlsChromiumClient:
			timeout = client.timeout
		default:
			t.Errorf("inventory client %d = %T, want a command client", index, client)
			continue
		}
		if timeout != expected.timeout {
			t.Errorf("image client %s timeout = %s, want inventory timeout %s", expected.name, timeout, expected.timeout)
		}
		// An optional client passes an image that lacks it, so a floor client must never be one.
		if optional != expected.optional {
			t.Errorf("image client %s optional = %v, want %v", expected.name, optional, expected.optional)
		}
	}
	if workerTLSClients[len(workerTLSClients)-1].timeout <= tlsDefaultProbeTimeout {
		t.Errorf("Chromium timeout %s must exceed the default %s", workerTLSClients[len(workerTLSClients)-1].timeout, tlsDefaultProbeTimeout)
	}
}

// optionalTLSClientNames names each optional client of the image.
var optionalTLSClientNames = []string{"gcloud", "bq", "az", "mise", "socat"}

// imageTLSClientsOf returns the image's clients of type T by name, built as the probe builds them
// under env with stateDir as their state directory, failing t unless each of names is one of them.
// Every image client is a tlsCommandClient underneath, so for that T it returns every client, an
// optional or Chromium client as the command client it embeds, whose command line it runs.
func imageTLSClientsOf[T tlsClient](t *testing.T, env []string, stateDir string, names ...string) map[string]T {
	t.Helper()
	clients := map[string]T{}
	add := func(client any) {
		if typed, ok := client.(T); ok {
			clients[typed.name()] = typed
		}
	}
	for _, client := range imageTLSClients("omp", env, stateDir) {
		switch client := client.(type) {
		case T:
			add(client)
		case tlsOptionalClient:
			add(client.tlsCommandClient)
		case tlsChromiumClient:
			add(client.tlsCommandClient)
		}
	}
	for _, name := range names {
		if _, ok := clients[name]; !ok {
			var zero T
			t.Fatalf("imageTLSClients() has no %T named %s", zero, name)
		}
	}
	return clients
}

// tlsSiteHosts is each tlsSites site's host name, written out rather than derived from its URL as
// the clients derive it.
var tlsSiteHosts = map[tlsSite]string{
	emSignTLSSite:  "dv-g3b-emudhra.we-have-a-problem.com",
	aaaTLSSite:     "dv-g3b-aaa-emudhra.we-have-a-problem.com",
	controlTLSSite: "example.com",
}

// wget, git, go, kubectl, redis-cli and uv are each classified from the output and exit status
// their program gave: the control's answer is an acceptance and the emSign site's refusal a
// refusal. git, go, redis-cli and uv fail at the control as well, so each needs the response
// marker its client reads, and the same marker reads the answer each gives at a compromised site
// whose chain it accepted. uv's control and refusal rows were measured on 2026-11-18 with uv
// 0.13.5 at example.com and the emSign site, and its accepting row on the image of 841974e9
// (sha256:37018eaa…, uv 0.13.5) in `legion probe-image`: an image whose /etc/uv/uv.toml allows
// the emSign and AAA hosts made uv accept both chains. At 841974e9, before uvResponseSeen read
// "Unsupported `Content-Type`", the probe failed naming uv as unable to verify a refusal rather
// than as accepting the chain, because the site's text/plain page was not an answer
// uvResponseSeen read.
func TestImageFloorTLSClientsClassifyTheirMeasuredOutput(t *testing.T) {
	clients := imageTLSClientsOf[tlsCommandClient](t, nil, t.TempDir(), "wget", "git", "go", "kubectl", "redis-cli", "uv")
	for _, testCase := range []struct {
		client, name, stderr, exit string
		want                       tlsStatus
	}{
		{client: "wget", name: "control", exit: "0", stderr: wgetControlAnswer, want: tlsConnectionAccepted},
		{client: "wget", name: "refusal", exit: "5", stderr: wgetEmSignRefusal, want: tlsCertificateRefused},
		{client: "git", name: "control", exit: "128", stderr: gitControlAnswer, want: tlsConnectionAccepted},
		{client: "git", name: "refusal", exit: "128", stderr: gitEmSignRefusal, want: tlsCertificateRefused},
		{client: "git", name: "accepting a compromised chain", exit: "128", stderr: gitEmSignAccepted, want: tlsConnectionAccepted},
		{client: "go", name: "control", exit: "1", stderr: goControlAnswer, want: tlsConnectionAccepted},
		{client: "go", name: "refusal", exit: "1", stderr: goEmSignRefusal, want: tlsCertificateRefused},
		{client: "go", name: "accepting a compromised chain", exit: "1", stderr: goEmSignAccepted, want: tlsConnectionAccepted},
		{client: "kubectl", name: "control", exit: "0", want: tlsConnectionAccepted},
		{client: "kubectl", name: "refusal", exit: "1", stderr: kubectlEmSignRefusal, want: tlsCertificateRefused},
		{client: "redis-cli", name: "control", exit: "1", stderr: redisControlAnswer, want: tlsConnectionAccepted},
		{client: "redis-cli", name: "refusal", exit: "1", stderr: redisEmSignRefusal, want: tlsCertificateRefused},
		{client: "redis-cli", name: "accepting a compromised chain", exit: "1", stderr: redisEmSignAccepted, want: tlsConnectionAccepted},
		{client: "uv", name: "control", exit: "1", stderr: "Using Python 3.13.5 environment at: /usr\nerror: No solution found when resolving dependencies\n  cause: Because legion-tls-probe-not-a-package was not found in the package registry and you require legion-tls-probe-not-a-package, we can conclude that your requirements are unsatisfiable.", want: tlsConnectionAccepted},
		{client: "uv", name: "refusal", exit: "2", stderr: "Using Python 3.13.5 environment at: /usr\nerror: Failed to fetch: https://dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe-not-a-package/\n  cause: error sending request for url (https://dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe-not-a-package/)\n  cause: client error (Connect)\n  cause: invalid peer certificate: Other(OtherError(CertDistrusted))\n\nhint: Consider enabling use of system TLS certificates with the `--system-certs` command-line flag", want: tlsCertificateRefused},
		{client: "uv", name: "accepting a compromised chain", exit: "1", stderr: "Using Python 3.13.5 environment at: /usr\nerror: Unsupported `Content-Type` \"text/plain\" for `https://dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe-not-a-package/`. Expected JSON or HTML.", want: tlsConnectionAccepted},
	} {
		t.Run(testCase.client+" "+testCase.name, func(t *testing.T) {
			client := clients[testCase.client]
			client.command = func(tlsSite) (string, []string, []string) {
				return "sh", []string{"-c", `printf %s "$1" >&2; exit "$2"`, "sh", testCase.stderr, testCase.exit}, nil
			}

			if outcome := client.probe(context.Background(), controlTLSSite, tlsRunTerms{}); outcome.status != testCase.want {
				t.Fatalf("%s probe = %+v, want status %q", testCase.client, outcome, testCase.want)
			}
		})
	}
}

// No image client but bun and omp, which read the package.json and tsconfig.json of each directory
// above theirs, reads configuration from a directory above the state directory newTLSStateDir
// makes, and none reads it from the probe's working directory. What each guard keeps its client
// from, with the measurement at that client's spec in workerTLSClients:
//
//   - every client runs in the state directory: each, from the probe's working directory;
//   - the state directory's go.mod: go, from a parent directory's go.mod;
//   - --no-config as mise's first argument: mise, from a parent directory's mise.toml;
//   - the state directory's workspace pyproject.toml: uv, from a parent directory's uv.toml and
//     pyproject.toml;
//   - GIT_CEILING_DIRECTORIES at the state directory's parent: git, from a parent repository.
func TestImageTLSClientsReadNoConfigurationAboveTheStateDirectory(t *testing.T) {
	stateDir, remove, err := newTLSStateDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
	isModule := func(line string) bool { return strings.HasPrefix(line, "module ") }
	if goMod, err := os.ReadFile(filepath.Join(stateDir, "go.mod")); err != nil || !slices.ContainsFunc(strings.Split(string(goMod), "\n"), isModule) {
		t.Errorf("the state directory's go.mod is %q, %v; want one holding a module directive", goMod, err)
	}
	if pyproject, err := os.ReadFile(filepath.Join(stateDir, "pyproject.toml")); err != nil || !slices.Contains(strings.Split(string(pyproject), "\n"), "[tool.uv.workspace]") {
		t.Errorf("the state directory's pyproject.toml is %q, %v; want one holding a [tool.uv.workspace] line", pyproject, err)
	}
	clients := imageTLSClientsOf[tlsCommandClient](t, nil, stateDir, workerTLSClientNames()...)
	for _, client := range clients {
		if client.workDir != stateDir {
			t.Errorf("%s runs in %q, want the state directory %s", client.name(), client.workDir, stateDir)
		}
	}
	ceiling := "GIT_CEILING_DIRECTORIES=" + filepath.Dir(stateDir)
	for _, site := range tlsSites {
		if _, args, _ := clients["mise"].commandLine(site); len(args) == 0 || args[0] != "--no-config" {
			t.Errorf("mise at %s runs %q, want --no-config first", site.name, args)
		}
		if _, _, env := clients["git"].commandLine(site); !slices.Contains(env, ceiling) {
			t.Errorf("git at %s runs under %q, want %s", site.name, env, ceiling)
		}
	}
}

// The state directory's path is absolute and holds no colon, since git skips a relative
// GIT_CEILING_DIRECTORIES entry and splits the value at colons: a relative TMPDIR is made absolute,
// and a TMPDIR holding a colon is refused, with an error naming the path and no directory left.
func TestTLSStateDirIsAbsoluteAndHoldsNoColon(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("relative", 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", "relative")
	stateDir, remove, err := newTLSStateDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
	if !filepath.IsAbs(stateDir) {
		t.Errorf("with TMPDIR=relative the state directory is %q, want an absolute path", stateDir)
	}

	colon := filepath.Join(t.TempDir(), "a:b")
	if err := os.Mkdir(colon, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", colon)
	if stateDir, remove, err := newTLSStateDir(); err == nil {
		remove()
		t.Errorf("with TMPDIR=%s the state directory is %s, want an error: git reads a colon as a separator in GIT_CEILING_DIRECTORIES", colon, stateDir)
	} else if !strings.Contains(err.Error(), colon) {
		t.Errorf("with TMPDIR=%s the error is %q, want it to name the path", colon, err)
	}
	if entries, err := os.ReadDir(colon); err != nil || len(entries) != 0 {
		t.Errorf("after the refusal TMPDIR=%s holds %v, %v; want it empty", colon, entries, err)
	}
}

// Every path go writes is under the probe's state directory, which it runs in and whose go.mod it
// reads (TestImageTLSClientsReadNoConfigurationAboveTheStateDirectory); it fetches the module path
// from the site's host itself, never downloads a toolchain and runs no authentication command. No
// KUBECONFIG, KUBERNETES_SERVICE_HOST or KUBERNETES_SERVICE_PORT of the pod's reaches kubectl: it
// reads its default configuration under its own empty home, and client-go never takes the
// in-cluster configuration, which would send the pod's service-account token to a test site. The
// pod's values below are ones each client's own environment must override or drop.
func TestImageGoAndKubectlTLSClientsKeepToTheStateDirectory(t *testing.T) {
	stateDir, remove, err := newTLSStateDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
	pod := []string{
		"HOME=/home/legion", "XDG_CONFIG_HOME=/home/legion/.config", "GOPATH=/home/legion/go",
		"GOMODCACHE=/legion/go/mod", "GOCACHE=/legion/go/build", "GOPROXY=https://proxy.golang.org",
		"GOTOOLCHAIN=auto", "GOFLAGS=-mod=mod", "GOWORK=/legion/go.work", "GOAUTH=netrc",
		"KUBECONFIG=/home/legion/.kube/config", "KUBERNETES_SERVICE_HOST=192.0.2.1", "KUBERNETES_SERVICE_PORT=443",
	}
	clients := imageTLSClientsOf[tlsCommandClient](t, pod, stateDir, "go", "kubectl")
	goClient, kubectl := clients["go"], clients["kubectl"]
	inStateDir := func(path string) bool { return strings.HasPrefix(path, stateDir+string(os.PathSeparator)) }

	for site, host := range tlsSiteHosts {
		program, args, env := goClient.commandLine(site)
		if want := []string{"mod", "download", host + "/legion-tls-probe@latest"}; program != "go" || !slices.Equal(args, want) {
			t.Errorf("go command for %s = %s %q, want go %q", site.name, program, args, want)
		}
		for _, name := range []string{"XDG_CONFIG_HOME", "GOPATH", "GOMODCACHE", "GOCACHE"} {
			if value := envValue(env, name); !inStateDir(value) {
				t.Errorf("go at %s has %s %q, want a path under %s", site.name, name, value, stateDir)
			}
		}
		for name, want := range map[string]string{"GOPROXY": "direct", "GOTOOLCHAIN": "local", "GOFLAGS": "", "GOWORK": "off", "GOAUTH": "off"} {
			if got := envValue(env, name); got != want {
				t.Errorf("go at %s has %s=%q, want %q", site.name, name, got, want)
			}
		}

		program, args, env = kubectl.commandLine(site)
		if want := []string{"--server=" + site.url, "--token=" + tlsProbeSignIn, "get", "--raw", "/"}; program != "kubectl" || !slices.Equal(args, want) {
			t.Errorf("kubectl command for %s = %s %q, want kubectl %q", site.name, program, args, want)
		}
		for _, name := range []string{"KUBECONFIG", "KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT"} {
			if slices.ContainsFunc(env, func(pair string) bool { return tlsVariableName(pair) == name }) {
				t.Errorf("kubectl at %s has %s=%q, want it absent so kubectl reads its default configuration under its own home and client-go never takes the in-cluster configuration", site.name, name, envValue(env, name))
			}
		}
	}
}

// redis-cli connects to the site's host, names that host in its SNI, and uses the https port,
// since no site's URL names another; it sends PING, which no site answers as Redis. It runs under
// three variables and no other: the kept PATH with the pod's value, a HOME of its own in the state
// directory, and LEGION_TLS_URL naming the site. REDISCLI_AUTH, the credential it would send as
// AUTH, is dropped with every other variable of the pod's.
func TestImageRedisCLITLSClientTargetsTheSitesHost(t *testing.T) {
	stateDir := t.TempDir()
	pod := []string{"HOME=/home/legion", "REDISCLI_AUTH=pod-credential", "PATH=/usr/bin"}
	client := imageTLSClientsOf[tlsCommandClient](t, pod, stateDir, "redis-cli")["redis-cli"]
	for site, host := range tlsSiteHosts {
		program, args, env := client.commandLine(site)
		if want := []string{"--tls", "--sni", host, "-h", host, "-p", "443", "PING"}; program != "redis-cli" || !slices.Equal(args, want) {
			t.Errorf("redis-cli command for %s = %s %q, want redis-cli %q", site.name, program, args, want)
		}
		if slices.ContainsFunc(env, func(pair string) bool { return tlsVariableName(pair) == "REDISCLI_AUTH" }) {
			t.Errorf("redis-cli at %s has REDISCLI_AUTH=%q, want it absent: redis-cli sends it as AUTH to the site", site.name, envValue(env, "REDISCLI_AUTH"))
		}
		want := slices.Sorted(slices.Values([]string{"PATH=/usr/bin", "HOME=" + filepath.Join(stateDir, "redis-cli"), "LEGION_TLS_URL=" + site.url}))
		if got := slices.Sorted(slices.Values(env)); !slices.Equal(got, want) {
			t.Errorf("redis-cli at %s runs under %q, want %q in any order", site.name, got, want)
		}
	}
}

// No client carries a credential of the pod's to a site. The pod below holds, with a value of the
// shape a real one has, every variable the probe once unset because a probed client reads a
// credential from it or from the file it names, and four that list missed. Among them, measured on
// 2026-11-18: gh 2.98.0's `gh auth token --hostname <host>` printed GH_ENTERPRISE_TOKEN or
// GITHUB_ENTERPRISE_TOKEN, which `gh api https://<host>/` would send; git sent an http.extraHeader
// given through GIT_CONFIG_PARAMETERS or GIT_CONFIG_COUNT; and redis-cli sent REDISCLI_AUTH as
// AUTH. The four the list missed were measured on the pull request's image (sha256:8f30d2f0…, with
// the legion of 1af3e416) against a local listener, each client run as the probe then ran it: GNU
// wget 1.25.0 sent the `header = Authorization: Bearer …` of the wgetrc SYSTEM_WGETRC named, git
// 2.47.3 the http.extraHeader of the configuration GIT_CONFIG_SYSTEM named with `git ls-remote`,
// gcloud 594.0.0 the contents of the file CLOUDSDK_AUTH_AUTHORIZATION_TOKEN_FILE named as
// x-goog-iam-authorization-token to the compute endpoint, and mise 2026.8.12's http: backend the
// `default` login of the netrc MISE_NETRC_FILE named as Basic at its first request; without the
// variable, none sent such a header. client-go takes the in-cluster configuration, with the pod's
// service-account token, from KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT, and the SDK
// reads CLOUDSDK_AUTH_ACCESS_TOKEN before its token file (credentials/store.py of SDK 589). The pod
// also holds a sentinel no list names, with a value made at run time, which stands for a variable
// nobody has found yet. At every site, each client's environment holds none of those names and
// none of those values, but for a name the client sets itself to a value of its own, and its HOME
// is in the state directory, so no netrc, rc file or configuration of the pod's home is read
// through HOME; curl, which also reads its passwd home, takes -q (TestTLSProbeCurlReadsNoCurlrc).
func TestImageTLSClientsCarryNoCredentialOfThePods(t *testing.T) {
	githubToken := func(seed string) string { return "ghp_" + strings.Repeat(seed, 9) }
	pod := map[string]string{
		"CLOUDSDK_AUTH_ACCESS_TOKEN":                "ya29." + strings.Repeat("a0Zb1Yc2", 8),
		"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE":           "/var/run/secrets/google/access-token",
		"CLOUDSDK_AUTH_AUTHORIZATION_TOKEN_FILE":    "/var/run/secrets/google/iam-authorization-token",
		"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE":    "/var/run/secrets/google/credentials.json",
		"CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT": "deployer@legion-prod.iam.gserviceaccount.com",
		"CLOUDSDK_CORE_ACCOUNT":                     "operator@legion-prod.iam.gserviceaccount.com",
		"CLOUDSDK_ACTIVE_CONFIG_NAME":               "legion-prod",
		"GOOGLE_APPLICATION_CREDENTIALS":            "/var/run/secrets/google/application_default_credentials.json",
		"BIGQUERYRC":                                "/home/legion/.bigqueryrc",
		"GH_TOKEN":                                  githubToken("0aZ9"),
		"GITHUB_TOKEN":                              githubToken("1bY8"),
		"GH_ENTERPRISE_TOKEN":                       githubToken("2cX7"),
		"GITHUB_ENTERPRISE_TOKEN":                   githubToken("3dW6"),
		"MISE_GITHUB_TOKEN":                         githubToken("4eV5"),
		"GITHUB_API_TOKEN":                          githubToken("5fU4"),
		"GIT_CONFIG_PARAMETERS":                     "'http.extraheader'='Authorization: Bearer " + githubToken("6gT3") + "'",
		"GIT_CONFIG_COUNT":                          "4",
		"GIT_ASKPASS":                               "/opt/legion/bin/git-askpass",
		"SSH_ASKPASS":                               "/usr/lib/ssh/x11-ssh-askpass",
		"GOAUTH":                                    "git /home/legion/src",
		"NETRC":                                     "/home/legion/.netrc",
		"MISE_NETRC_FILE":                           "/home/legion/.config/mise/netrc",
		"KUBERNETES_SERVICE_HOST":                   "10.96.0.1",
		"KUBERNETES_SERVICE_PORT":                   "443",
		"KUBECONFIG":                                "/var/run/secrets/kubernetes.io/serviceaccount/kubeconfig",
		"REDISCLI_AUTH":                             "redis-" + strings.Repeat("9f8e7d6c", 4),
		"AWS_ACCESS_KEY_ID":                         "AKIA" + strings.Repeat("Q7", 8),
		"AWS_SECRET_ACCESS_KEY":                     strings.Repeat("wJa1/K7Md", 4) + "EnGb",
		"AWS_SESSION_TOKEN":                         "IQoJb3JpZ2luX2Vj" + strings.Repeat("EJr/wEaCXVz", 4),
		"AWS_PROFILE":                               "legion-admin",
		"AWS_ROLE_ARN":                              "arn:aws:iam::111122223333:role/legion-worker",
		"AWS_WEB_IDENTITY_TOKEN_FILE":               "/var/run/secrets/eks.amazonaws.com/serviceaccount/token",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI":        "http://169.254.170.23/v1/credentials",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI":    "/v2/credentials/5f1d2a3b-legion",
		"AWS_CONTAINER_AUTHORIZATION_TOKEN":         strings.Repeat("c2VjcmV0", 5),
		"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE":    "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount/eks-pod-identity-token",
		"CURL_HOME":                                 "/home/legion/.curl-credentials",
		"WGETRC":                                    "/home/legion/.wgetrc",
		"SYSTEM_WGETRC":                             "/home/legion/.config/wget/system-wgetrc",
		"UV_CONFIG_FILE":                            "/home/legion/.config/uv/uv.toml",
		"GIT_CONFIG_GLOBAL":                         "/home/legion/.gitconfig-credentials",
		"GIT_CONFIG_SYSTEM":                         "/home/legion/.config/git/system-config",
		"LEGION_TLS_PROBE_SENTINEL":                 "sentinel-" + rand.Text(),
	}
	// setItself is each name a client sets itself, to a value of its own.
	setItself := map[string][]string{
		"gh":     {"GH_TOKEN"},
		"go":     {"GOAUTH"},
		"gcloud": {"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE"},
		"bq":     {"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE"},
	}
	env := []string{"HOME=/home/legion", "PATH=/usr/bin:/bin"}
	for name, value := range pod {
		env = append(env, name+"="+value)
	}
	stateDir, remove, err := newTLSStateDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)

	for _, client := range imageTLSClientsOf[tlsCommandClient](t, env, stateDir, workerTLSClientNames()...) {
		for _, site := range tlsSites {
			_, _, environment := client.commandLine(site)
			for _, pair := range environment {
				name, value, _ := strings.Cut(pair, "=")
				if _, credential := pod[name]; credential && !slices.Contains(setItself[client.name()], name) {
					t.Errorf("%s at %s carries the pod's %s", client.name(), site.name, name)
				}
				for credentialName, credential := range pod {
					if value == credential {
						t.Errorf("%s at %s carries the pod's %s value in %s", client.name(), site.name, credentialName, name)
					}
				}
			}
			if home := envValue(environment, "HOME"); !strings.HasPrefix(home, stateDir+string(os.PathSeparator)) {
				t.Errorf("%s at %s has HOME %q, want a directory under the state directory %s", client.name(), site.name, home, stateDir)
			}
		}
	}
}

// Every client keeps every variable the worker image declares, with the value the probe's
// environment gives it, but for HOME, which every client has of its own in the state directory
// (TestImageTLSClientsCarryNoCredentialOfThePods). So the probe cannot drop a variable of the
// image's that may set the trust store, as AWS_CA_BUNDLE does, and with
// TestImageEnvIsWhatTheWorkerImageSets a new ENV of the image's final stage is kept too.
func TestImageTLSClientsKeepEveryVariableTheImageDeclares(t *testing.T) {
	declared := map[string]string{}
	env := make([]string, 0, len(sandbox.ImageEnv))
	for index, name := range sandbox.ImageEnv {
		declared[name] = fmt.Sprintf("legion-tls-probe-image-%d", index)
		env = append(env, name+"="+declared[name])
	}

	for _, client := range imageTLSClientsOf[tlsCommandClient](t, env, t.TempDir(), workerTLSClientNames()...) {
		for _, site := range tlsSites {
			_, _, environment := client.commandLine(site)
			for name, want := range declared {
				if name == "HOME" {
					continue
				}
				if got := envValue(environment, name); got != want {
					t.Errorf("%s at %s has %s=%q, want the image's %q", client.name(), site.name, name, got, want)
				}
			}
		}
	}
}

// Every client keeps each trust and verification setting of the pod's, with the pod's value, so the
// probe holds the image's clients to the trust the roles in the pod get. No client sets one itself,
// so each reaches every client at every site unchanged.
func TestImageTLSClientsKeepEveryTrustSettingOfThePods(t *testing.T) {
	pod := map[string]string{}
	env := []string{"HOME=/home/legion", "PATH=/usr/bin:/bin"}
	for index, name := range tlsTrustVariables {
		pod[name] = fmt.Sprintf("legion-tls-probe-trust-%d", index)
		env = append(env, name+"="+pod[name])
	}

	for _, client := range imageTLSClientsOf[tlsCommandClient](t, env, t.TempDir(), workerTLSClientNames()...) {
		for _, site := range tlsSites {
			_, _, environment := client.commandLine(site)
			for name, want := range pod {
				if got := envValue(environment, name); got != want {
					t.Errorf("%s at %s has %s=%q, want the pod's %q", client.name(), site.name, name, got, want)
				}
			}
		}
	}
}

// A trust setting of the pod's that makes the roles' clients accept the emSign chain fails the
// probe: NODE_TLS_REJECT_UNAUTHORIZED=0 for the clients that read it, and SSL_CERT_FILE naming a
// file that holds the emSign root for those on every image that read OpenSSL's or Go's store. Each
// client runs a stand-in in place of its program and arguments, under the environment its
// commandLine computes from the pod's and its own command's: the stand-in reaches the control, and
// at a compromised site it accepts where that environment carries the setting, for SSL_CERT_FILE
// any path, since the probe never reads the file it names, and refuses for a certificate error
// otherwise. Without the setting the same clients pass, so the test is not blind: a probe that
// dropped the setting would pass a pod that sets it.
func TestImageTLSProbeFailsAPodTrustSettingThatAcceptsTheEmSignChain(t *testing.T) {
	pod := []string{"HOME=/home/legion", "PATH=/usr/bin:/bin"}
	for _, testCase := range []struct {
		name, setting, accepts string
		clients                []string
	}{
		{
			name:    "NODE_TLS_REJECT_UNAUTHORIZED=0",
			setting: "NODE_TLS_REJECT_UNAUTHORIZED=0",
			accepts: `[ "${NODE_TLS_REJECT_UNAUTHORIZED-}" = 0 ]`,
			clients: []string{"node", "bun", "omp", "codegraph-node"},
		},
		{
			name:    "SSL_CERT_FILE naming the emSign root",
			setting: "SSL_CERT_FILE=/home/legion/bundle-with-emsign.pem",
			accepts: `[ -n "${SSL_CERT_FILE-}" ]`,
			clients: []string{"curl", "python3", "gh", "go", "kubectl", "redis-cli"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			script := `if [ "$1" = control ] || { ` + testCase.accepts + `; }; then exit 0; fi; echo certificate verify failed >&2; exit 1`
			standIns := func(env []string) []tlsClient {
				image := imageTLSClientsOf[tlsCommandClient](t, env, t.TempDir(), testCase.clients...)
				clients := make([]tlsClient, 0, len(testCase.clients))
				for _, name := range testCase.clients {
					client := image[name]
					command := client.command
					client.command = func(site tlsSite) (string, []string, []string) {
						_, _, set := command(site)
						kind := "compromised"
						if site.control {
							kind = "control"
						}
						return "sh", []string{"-c", script, "sh", kind}, set
					}
					clients = append(clients, client)
				}
				return clients
			}

			_, _, err := probeTLSClients(context.Background(), standIns(append(slices.Clone(pod), testCase.setting)), compromisedTLSSites())
			for _, name := range testCase.clients {
				if want := name + " accepts the emSign chain"; err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("probeTLSClients() under a pod env with %s = %v, want an error containing %q", testCase.setting, err, want)
				}
			}
			if _, _, err := probeTLSClients(context.Background(), standIns(pod), compromisedTLSSites()); err != nil {
				t.Errorf("probeTLSClients() under a pod env without %s = %v, want success", testCase.setting, err)
			}
		})
	}
}

// tlsEnvironment takes explicit lists: an entry of base whose name keep holds stays, every other
// entry of base goes, an entry of set replaces base's value of its name, a later entry of set
// replaces an earlier one of its name, and an entry of set that is not NAME=value is refused,
// never read as anything.
func TestTLSEnvironmentKeepsAndSetsByExplicitLists(t *testing.T) {
	base := []string{"HOME=/home/legion", "GOFLAGS=-mod=mod", "REDISCLI_AUTH=pod-credential", "PATH=/usr/bin"}
	kept := []string{"HOME", "PATH", "USER"}
	for _, testCase := range []struct {
		name            string
		keep, set, want []string
	}{
		{name: "kept names stay and others go", keep: kept, want: []string{"HOME=/home/legion", "PATH=/usr/bin"}},
		{name: "nothing kept", want: []string{}},
		{name: "set replaces a kept name", keep: kept, set: []string{"HOME=/state/go"}, want: []string{"PATH=/usr/bin", "HOME=/state/go"}},
		{name: "set gives a name keep lacks", keep: kept, set: []string{"GOFLAGS="}, want: []string{"HOME=/home/legion", "PATH=/usr/bin", "GOFLAGS="}},
		{name: "a later set entry replaces an earlier one", keep: kept, set: []string{"HOME=/state/curl", "GIT_TERMINAL_PROMPT=0", "HOME=/state/go"}, want: []string{"PATH=/usr/bin", "GIT_TERMINAL_PROMPT=0", "HOME=/state/go"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := tlsEnvironment(base, testCase.keep, testCase.set); !slices.Equal(got, testCase.want) {
				t.Fatalf("tlsEnvironment(%q, %q, %q) = %q, want %q", base, testCase.keep, testCase.set, got, testCase.want)
			}
		})
	}
	t.Run("a set entry without = is refused", func(t *testing.T) {
		defer func() {
			if recovered := recover(); recovered == nil || !strings.Contains(fmt.Sprint(recovered), `"REDISCLI_AUTH"`) {
				t.Fatalf("tlsEnvironment with the set entry REDISCLI_AUTH recovered %v, want a panic naming the entry", recovered)
			}
		}()
		tlsEnvironment(base, kept, []string{"REDISCLI_AUTH"})
	})
}

// Each optional client is classified from the output its program gave in an INFRA issue pod on
// 2026-11-18 (gcloud 594.0.0, bq 2.1.42, azure-cli 2.93.0, mise 2026.8.12, socat 1.8.0.3): a page
// the site answered, or for socat a handshake it completed, is an acceptance, a certificate refusal
// a refusal, and an error before any answer neither. bq writes its error to stdout, so its
// stand-in does too.
func TestImageOptionalTLSClientsClassifyTheirMeasuredOutput(t *testing.T) {
	clients := imageTLSClientsOf[tlsOptionalClient](t, nil, t.TempDir(), optionalTLSClientNames...)
	for _, testCase := range []struct {
		client, name, stdout, stderr, exit string
		want                               tlsStatus
	}{
		{client: "gcloud", name: "control page", exit: "1", want: tlsConnectionAccepted,
			stderr: `ERROR: (gcloud.compute.regions.list) Some requests did not succeed: - <!doctype html><html lang=en>...Example Domain...`},
		{client: "gcloud", name: "emSign page through an SDK that accepts the chain", exit: "1", want: tlsConnectionAccepted,
			stderr: `Error decoding response "Your client successfully connected to the site. That's bad!"`},
		{client: "gcloud", name: "emSign page through the stock SDK, which crashes on it", exit: "1", want: tlsConnectionAccepted,
			stderr: `ERROR: gcloud crashed (JSONDecodeError): Expecting value: line 1 column 1 (char 0) If you would like to report this issue, please run the following command: gcloud feedback`},
		{client: "gcloud", name: "refusal", exit: "1", want: tlsCertificateRefused,
			stderr: `ERROR: gcloud crashed (SSLError): HTTPSConnectionPool(host='dv-g3b-emudhra.we-have-a-problem.com', port=443): Max retries exceeded with url: /compute/v1/projects/legion-tls-probe/regions?alt=json&maxResults=500 (Caused by SSLError(SSLCertVerificationError(1, '[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1082)'))) gcloud's default CA certificates failed to verify your connection,`},
		{client: "gcloud", name: "unresolved host", exit: "1", want: tlsNetworkUnavailable,
			stderr: `ERROR: gcloud crashed (ConnectionError): HTTPSConnectionPool(host='example.invalid', port=443): Max retries exceeded with url: /compute/v1/projects/legion-tls-probe/regions?alt=json&maxResults=500 (Caused by NameResolutionError("<urllib3.connection.HTTPSConnection object>: Failed to resolve 'example.invalid' ([Errno -2] Name or service not known)"))`},
		{client: "bq", name: "control 404", exit: "1", want: tlsConnectionAccepted,
			stdout: `BigQuery error in ls operation: Got 404 response from discovery url: https://example.com/$discovery/rest?version=v2`},
		{client: "bq", name: "corrupt discovery document", exit: "1", want: tlsConnectionAccepted,
			stdout: `BigQuery error in ls operation: Received corrupt "bigquery" discovery doc`},
		{client: "bq", name: "discovery document without resources", exit: "1", want: tlsConnectionAccepted,
			stdout: `BigQuery error in ls operation: Discovery document from https://example.com/$discovery/rest?version=v2 for "bigquery" contains no resources.`},
		{client: "bq", name: "emSign page through the stock SDK, which bq reports as a bug", exit: "1", want: tlsConnectionAccepted,
			stdout: `You have encountered a bug in the BigQuery CLI. Please file a bug report in our public issue tracker: == Error trace == raise JSONDecodeError("Expecting value", s, err.value) from None json.decoder.JSONDecodeError: Expecting value: line 1 column 1 (char 0)`},
		{client: "bq", name: "refusal", exit: "1", want: tlsCertificateRefused,
			stdout: `BigQuery error in ls operation: Cannot contact server. Please try again. Error: SSLCertVerificationError(1, '[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1082)')`,
			stderr: `RequestsDependencyWarning: urllib3 (1.26.9) or chardet ... doesn't match a supported version!`},
		{client: "bq", name: "no active account", exit: "1", want: tlsNetworkUnavailable,
			stderr: `You do not currently have an active account selected.`},
		{client: "az", name: "control page", exit: "0", want: tlsConnectionAccepted,
			stdout: `<!doctype html><html lang="en"><head><title>Example Domain</title>`,
			stderr: `WARNING: Not a json response, outputting to stdout. For binary data suggest use "--output-file" to write to a file`},
		{client: "az", name: "refusal", exit: "1", want: tlsCertificateRefused,
			stderr: `ERROR: HTTPSConnectionPool(host='dv-g3b-emudhra.we-have-a-problem.com', port=443): Max retries exceeded with url: / (Caused by SSLError(SSLCertVerificationError(1, '[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1032)'))) Certificate verification failed. This typically happens when using Azure CLI behind a proxy`},
		{client: "mise", name: "control 200", exit: "1", want: tlsConnectionAccepted,
			stderr: `DEBUG Loaded 148 CA root certificates from the system DEBUG GET Downloading https://example.com/ to ~/data/downloads/http-legion-tls-probe/1/ DEBUG GET https://example.com/ DEBUG GET https://example.com/ 200 OK Error: 0: Failed to install http:legion-tls-probe@1: Not a directory (os error 20)`},
		{client: "mise", name: "refusal", exit: "1", want: tlsCertificateRefused,
			stderr: `DEBUG GET https://dv-g3b-emudhra.we-have-a-problem.com/ Error: 0: Failed to install http:legion-tls-probe@1: error sending request: client error (Connect): invalid peer certificate: UnknownIssuer`},
		{client: "mise", name: "refusal without MISE_VERBOSE", exit: "1", want: tlsCertificateRefused,
			stderr: `mise ERROR failed to verify TLS certificate: invalid peer certificate: UnknownIssuer`},
		{client: "mise", name: "unresolved host", exit: "1", want: tlsNetworkUnavailable,
			stderr: `DEBUG GET https://httpbin.invalid/ Error: 0: Failed to install http:legion-tls-probe@1: error sending request: client error (Connect): dns error: failed to lookup address information: Name or service not known`},
		{client: "socat", name: "control handshake", exit: "0", want: tlsConnectionAccepted,
			stderr: `2026/11/18 06:25:03 socat[36662] W OpenSSL: Warning: this implementation does not check CRLs`},
		{client: "socat", name: "refusal", exit: "255", want: tlsCertificateRefused,
			stderr: `2026/11/18 06:25:02 socat[36660] W OpenSSL: Warning: this implementation does not check CRLs 2026/11/18 06:25:03 socat[36660] W SSL_connect(): error:0A000086:SSL routines::certificate verify failed 2026/11/18 06:25:03 socat[36660] W no peer certificate`},
	} {
		t.Run(testCase.client+" "+testCase.name, func(t *testing.T) {
			client := clients[testCase.client].tlsCommandClient
			client.command = func(tlsSite) (string, []string, []string) {
				return "sh", []string{"-c", `printf %s "$1"; printf %s "$2" >&2; exit "$3"`, "sh", testCase.stdout, testCase.stderr, testCase.exit}, nil
			}

			if outcome := client.probe(context.Background(), controlTLSSite, tlsRunTerms{}); outcome.status != testCase.want {
				t.Fatalf("%s probe = %+v, want status %q", testCase.client, outcome, testCase.want)
			}
		})
	}
}

// An image built on another base may not carry an optional client, or may carry its link without
// the tree the link points into. Either way the client is absent at every site and fails nothing;
// on PATH, it is held to the chains as every client is. Each of the image's own optional clients is
// absent where PATH holds none of them.
func TestOptionalTLSClientIsAbsentOffPATHAndHeldToTheChainsOnIt(t *testing.T) {
	bin := t.TempDir()
	if err := os.Symlink(filepath.Join(bin, "gone"), filepath.Join(bin, "legion-tls-dangling")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	acceptingEverySite := func(program string) tlsClient {
		return tlsOptionalClient{program: program, tlsCommandClient: tlsCommandClient{
			clientName: "optional",
			timeout:    tlsDefaultProbeTimeout,
			command: func(tlsSite) (string, []string, []string) {
				return program, []string{"-c", "exit 0"}, nil
			},
		}}
	}

	for _, program := range []string{"legion-tls-probe-no-such-program", "legion-tls-dangling"} {
		observations, _, err := probeTLSClients(context.Background(), []tlsClient{acceptingEverySite(program)}, compromisedTLSSites())
		if err != nil {
			t.Errorf("probeTLSClients() with %s off PATH = %v, want success", program, err)
		}
		if len(observations) != len(tlsSites) {
			t.Fatalf("probeTLSClients() with %s off PATH returned %d observations, want one per site (%d)", program, len(observations), len(tlsSites))
		}
		for _, observation := range observations {
			if observation.status != tlsClientAbsent || observation.detail != program+" is not on PATH" {
				t.Errorf("%s at %s = %+v, want absent (%s is not on PATH)", program, observation.site.name, observation.tlsOutcome, program)
			}
		}
	}

	if _, _, err := probeTLSClients(context.Background(), []tlsClient{acceptingEverySite("sh")}, compromisedTLSSites()); err == nil || !strings.Contains(err.Error(), "optional accepts the emSign chain") {
		t.Fatalf("probeTLSClients() with the program on PATH accepting every chain = %v, want the emSign acceptance named", err)
	}

	optional := imageTLSClientsOf[tlsOptionalClient](t, nil, t.TempDir(), optionalTLSClientNames...)
	t.Setenv("PATH", bin)
	for name, client := range optional {
		for _, site := range tlsSites {
			if outcome := client.probe(context.Background(), site, tlsRunTerms{}); outcome.status != tlsClientAbsent || outcome.detail != name+" is not on PATH" {
				t.Errorf("%s at %s with no optional client on PATH = %+v, want absent (%s is not on PATH)", name, site.name, outcome, name)
			}
		}
	}
}

// gcloud reaches each site as its compute endpoint with its configuration in the probe's state
// directory, which holds the token it signs in with. mise names the site inside its one tool
// argument, and keeps each site's download apart. socat names the site's host and https port in
// its OPENSSL: address.
func TestImageOptionalTLSClientsTargetTheSiteFromTheStateDirectory(t *testing.T) {
	stateDir, remove, err := newTLSStateDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
	clients := imageTLSClientsOf[tlsOptionalClient](t, nil, stateDir, optionalTLSClientNames...)
	inStateDir := func(path string) bool { return strings.HasPrefix(path, stateDir+string(os.PathSeparator)) }
	miseData := map[string]bool{}

	for _, site := range tlsSites {
		_, _, env := clients["gcloud"].commandLine(site)
		if got, want := envValue(env, "CLOUDSDK_API_ENDPOINT_OVERRIDES_COMPUTE"), site.url+"compute/v1/"; got != want {
			t.Errorf("gcloud at %s has compute endpoint %q, want %q", site.name, got, want)
		}
		if config := envValue(env, "CLOUDSDK_CONFIG"); !inStateDir(config) {
			t.Errorf("gcloud at %s has CLOUDSDK_CONFIG %q, want a directory under %s", site.name, config, stateDir)
		}
		if token, err := os.ReadFile(envValue(env, "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE")); err != nil || len(token) == 0 {
			t.Errorf("gcloud at %s reads its access token from %q: %q, %v; want a token the state directory holds", site.name, envValue(env, "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE"), token, err)
		}

		_, args, env := clients["mise"].commandLine(site)
		if want := "http:legion-tls-probe[url=" + site.url + "]@1"; !slices.Contains(args, want) {
			t.Errorf("mise at %s runs %q, want the tool argument %q", site.name, args, want)
		}
		miseData[envValue(env, "MISE_DATA_DIR")] = true

		if _, args, _ := clients["socat"].commandLine(site); !slices.Equal(args, []string{"-u", "-", "OPENSSL:" + tlsSiteHosts[site] + ":443"}) {
			t.Errorf("socat at %s runs %q, want the address OPENSSL:%s:443", site.name, args, tlsSiteHosts[site])
		}
	}
	if len(miseData) != len(tlsSites) {
		t.Errorf("mise keeps %d data directories for %d sites, want one per site: %v", len(miseData), len(tlsSites), miseData)
	}
}

// Chromium resolves only the probed site, so its certificate verifier's AIA fetches fail at once
// instead of waiting on the CA's repository host.
func TestImageChromiumResolvesOnlyTheProbedSite(t *testing.T) {
	chromium := imageTLSClientsOf[tlsChromiumClient](t, nil, "", "chromium")["chromium"]
	for site, host := range tlsSiteHosts {
		program, args, _ := chromium.command(site)
		want := []string{"--headless", "--no-sandbox", "--host-resolver-rules=MAP * ^NOTFOUND, EXCLUDE " + host, "--dump-dom", site.url}
		if program != "chromium" || !slices.Equal(args, want) {
			t.Errorf("chromium command for %s = %s %q, want chromium %q", site.name, program, args, want)
		}
	}
}

// A positive control must accept its pinned root and its isolation endpoint must refuse it; any
// other outcome fails, and a run the probe's deadline ended has no verdict, which fails nothing and
// is named apart from the failures.
func TestTLSPositiveControlsRequireTheExpectedChain(t *testing.T) {
	cut := tlsOutcome{status: tlsNoVerdict, detail: "the probe's deadline ended it after 0.2s: signal: terminated"}
	for _, testCase := range []struct {
		name, want         string
		outcome, isolation tlsOutcome
		noVerdict          []string
	}{
		{name: "accepts pinned root", outcome: tlsOutcome{status: tlsConnectionAccepted}, isolation: tlsOutcome{status: tlsCertificateRefused}},
		{name: "refuses pinned root", outcome: tlsOutcome{status: tlsCertificateRefused, detail: "certificate verify failed"}, isolation: tlsOutcome{status: tlsCertificateRefused}, want: "emSign positive control"},
		{name: "cannot reach site", outcome: tlsOutcome{status: tlsNetworkUnavailable, detail: "network is unreachable"}, isolation: tlsOutcome{status: tlsCertificateRefused}, want: "emSign positive control"},
		{name: "cannot reach isolation endpoint", outcome: tlsOutcome{status: tlsConnectionAccepted}, isolation: tlsOutcome{status: tlsNetworkUnavailable, detail: "network is unreachable"}, want: "isolation endpoint"},
		{name: "the isolation endpoint cut by the probe's deadline", outcome: tlsOutcome{status: tlsConnectionAccepted}, isolation: cut,
			noVerdict: []string{"emSign positive control isolation endpoint " + isolationTLSSite.url + ": " + cut.detail}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			noVerdict, err := verifyTLSPositiveControls([]tlsSite{emSignTLSSite}, func(site tlsSite) tlsOutcome {
				if site.url == isolationTLSSite.url {
					return testCase.isolation
				}
				return testCase.outcome
			})
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("verifyTLSPositiveControls() = %v, want success", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("verifyTLSPositiveControls() = %v, want error containing %q", err, testCase.want)
			}
			if !slices.Equal(noVerdict, testCase.noVerdict) {
				t.Errorf("verifyTLSPositiveControls() runs with no verdict = %q, want %q", noVerdict, testCase.noVerdict)
			}
		})
	}
}

// Every curl the probe runs reads no curlrc: the curl client and the positive controls each reach a
// test site, and curl sends a curlrc's headers to every host it reaches. A HOME of its own does not
// keep curl from the pod's: where the curlrc is in none of CURL_HOME, XDG_CONFIG_HOME and HOME, curl
// 8.14.1 reads it from the home the passwd database names for its user (findfile, curl's
// tool_findfile.c). Measured on 2026-11-18 on the head's image (sha256:40cd7424…, the legion of
// d56fa735) against a local listener, with /home/legion/.curlrc holding an Authorization header: the
// curl client, run with HOME in the state directory, and the positive controls' curl, run with the
// pod's HOME, each sent that header. curl reads no curlrc only where -q (--disable) is its first
// argument (measured: `curl -q -s …` sent no header from a CURL_HOME curlrc, `curl -s -q …` sent it).
func TestTLSProbeCurlReadsNoCurlrc(t *testing.T) {
	curl := imageTLSClientsOf[tlsCommandClient](t, nil, t.TempDir(), "curl")["curl"]
	for _, site := range tlsSites {
		if _, args, _ := curl.commandLine(site); len(args) == 0 || (args[0] != "-q" && args[0] != "--disable") {
			t.Errorf("the curl client at %s runs curl %q, whose first argument is not -q", site.name, args)
		}
	}

	bin := t.TempDir()
	record := filepath.Join(bin, "first-arguments")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> '" + record + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// The stand-in curl accepts every site, the isolation endpoint included, so the controls fail;
	// only the arguments they ran curl with matter here.
	_ = probeTLSPositiveControls(context.Background(), []string{"PATH=" + os.Getenv("PATH")})
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the positive controls never ran curl: %v", err)
	}
	firsts := strings.Fields(string(data))
	if len(firsts) == 0 {
		t.Fatal("the positive controls never ran curl")
	}
	for _, first := range firsts {
		if first != "-q" && first != "--disable" {
			t.Errorf("a positive control ran curl with first argument %q, not -q", first)
		}
	}
}

// A TLS run the probe's deadline (--probe-budget) ends has no verdict, wherever it ran and whatever
// it printed: the probe reads no output of a run it did not let finish. It fails nothing and gets
// no second run, since the deadline has passed.
func TestTLSProbeRunCutByTheProbesDeadlineHasNoVerdict(t *testing.T) {
	originalSites := tlsSites
	tlsSites = []tlsSite{emSignTLSSite, aaaTLSSite}
	t.Cleanup(func() { tlsSites = originalSites })

	client := tlsCommandClient{
		clientName: "curl",
		timeout:    tlsDefaultProbeTimeout,
		command: func(site tlsSite) (string, []string, []string) {
			if site.name == emSignTLSSite.name {
				return "sh", []string{"-c", "echo certificate verify failed >&2; exit 1"}, nil
			}
			return "sh", []string{"-c", "exec sleep 30"}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	observations, noVerdict, err := probeTLSClients(ctx, []tlsClient{client}, compromisedTLSSites())
	if len(observations) != 2 {
		t.Fatalf("probeTLSClients() returned %d observations, want one run at each site and no second run: %+v", len(observations), observations)
	}
	if got := observations[1]; got.status != tlsNoVerdict || !strings.HasPrefix(got.detail, "the probe's deadline ended it after ") {
		t.Errorf("AAA outcome = %+v, want no verdict, the probe's deadline named", got.tlsOutcome)
	}
	if err != nil {
		t.Errorf("probeTLSClients() = %v, want no failure: a run the deadline ended fails nothing", err)
	}
	if len(noVerdict) != 1 || !strings.HasPrefix(noVerdict[0], "curl AAA: the probe's deadline ended it after ") {
		t.Errorf("probeTLSClients() runs with no verdict = %q, want curl's AAA run alone, the deadline named", noVerdict)
	}
}

func TestTLSPositiveControlsLineNamesEverySite(t *testing.T) {
	sites := []tlsSite{{name: "first"}, {name: "second"}, {name: "third"}}
	const want = "probe-image: TLS positive controls: first, second and third chains verified"

	if got := tlsPositiveControlsLine(sites); got != want {
		t.Fatalf("tlsPositiveControlsLine() = %q, want %q", got, want)
	}
}

func TestTLSCommandClientClassifiesOutputStreams(t *testing.T) {
	for _, testCase := range []struct {
		name, stdout, stderr, exit string
		want                       tlsStatus
	}{
		{name: "accepts a loaded body with certificate text", stdout: "page body NET::ERR_CERT_AUTHORITY_INVALID", want: tlsConnectionAccepted},
		{name: "refuses a certificate failure", stderr: "net::ERR_CERT_AUTHORITY_INVALID", exit: "failure", want: tlsCertificateRefused},
		{name: "reports another failure as unavailable", stderr: "network is unreachable", exit: "failure", want: tlsNetworkUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := tlsCommandClient{
				clientName: "generic",
				timeout:    tlsDefaultProbeTimeout,
				command: func(tlsSite) (string, []string, []string) {
					return "sh", []string{"-c", `printf %s "$1"; printf %s "$2" >&2; test "$3" != failure`, "sh", testCase.stdout, testCase.stderr, testCase.exit}, nil
				},
			}

			if outcome := client.probe(context.Background(), controlTLSSite, tlsRunTerms{}); outcome.status != testCase.want {
				t.Fatalf("client.probe() = %+v, want status %q", outcome, testCase.want)
			}
		})
	}
}

func TestTLSChromiumClientClassifiesNavigationResult(t *testing.T) {
	for _, testCase := range []struct {
		name, stdout, stderr string
		want                 tlsStatus
	}{
		{name: "accepts a loaded DOM with certificate text", stdout: "page body NET::ERR_CERT_AUTHORITY_INVALID", want: tlsConnectionAccepted},
		{name: "refuses a certificate navigation failure", stdout: "page body NET::ERR_CERT_AUTHORITY_INVALID", stderr: "Page load failed: net::ERR_CERT_AUTHORITY_INVALID", want: tlsCertificateRefused},
		{name: "refuses after failed AIA fetches", stderr: "[5:18:1028/173830.670117:ERROR:net/cert/internal/cert_issuer_source_aia.cc:138] AiaRequest::OnFetchCompleted got error -105\n[5:5:1028/173830.690777:ERROR:components/headless/command_handler/headless_command_handler.cc:403] Page load failed: net::ERR_CERT_AUTHORITY_INVALID\n", want: tlsCertificateRefused},
		{name: "reports another navigation failure as unavailable", stderr: "Page load failed: net::ERR_NAME_NOT_RESOLVED", want: tlsNetworkUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := tlsChromiumClient{tlsCommandClient: tlsCommandClient{
				clientName: "chromium",
				timeout:    tlsChromiumProbeTimeout,
				command: func(tlsSite) (string, []string, []string) {
					return "sh", []string{"-c", `printf %s "$1"; printf %s "$2" >&2`, "sh", testCase.stdout, testCase.stderr}, nil
				},
			}}

			outcome := client.probe(context.Background(), controlTLSSite, tlsRunTerms{})
			if outcome.status != testCase.want {
				t.Fatalf("client.probe() = %+v, want status %q", outcome, testCase.want)
			}
			if strings.Contains(outcome.detail, "page body") {
				t.Fatalf("client.probe() detail = %q, must not read the DOM", outcome.detail)
			}
		})
	}
}

// Chromium's first process in a probe pod needs more time than the other clients,
// while each ordinary client keeps the short refusal bound.
func TestImageTLSChromiumClientHasColdStartHeadroom(t *testing.T) {
	chromium := imageTLSClientsOf[tlsChromiumClient](t, nil, "", "chromium")["chromium"]
	chromium.command = func(tlsSite) (string, []string, []string) {
		return "sh", []string{"-c", "exec sleep 21"}, nil
	}

	outcome := chromium.probe(context.Background(), controlTLSSite, tlsRunTerms{})
	if outcome.status != tlsConnectionAccepted {
		t.Fatalf("Chromium probe after a 21s cold start = %+v, want accepted", outcome)
	}
}

// What node and aws printed at the TLS sites in the probe pods, which the stand-in clients below
// replay: node's refusal of a compromised chain (exit 1, on stderr), as INFRA's probe pod logged it
// on 2026-11-19 with the probe's trim; aws's refusal (exit 255, on stderr), which names the site's
// URL, and its answer at the control (exit 255), the page it could not parse, as BET's probe pod
// logged them on 2026-11-17. node exits 0 with no output at the control.
const (
	nodeTLSRefusal   = `node:internal/modules/run_main:107 triggerUncaughtException( ^ [TypeError: fetch fail…:34) at TLSSocket.emit (node:events:514:28) at TLSSocket._finishInit (node:internal/tls/wrap:1216:8) at ssl.onhandshakedone (node:internal/tls/wrap:997:12) { code: 'CERT_REVOKED' } } Node.js v24.22.1`
	awsTLSRefusal    = `aws: [ERROR]: SSL validation failed for %s [SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1082)`
	awsControlAnswer = `aws: [ERROR]: Unable to parse response (syntax error: line 1, column 0), invalid XML received. Further retries may succeed: <!doctype html><html lang="en"><head><title>Example Domain</title>`
)

// nodeAnswer is what node prints at site, and the status it exits with.
func nodeAnswer(site tlsSite) (string, int) {
	if site.control {
		return "", 0
	}
	return nodeTLSRefusal, 1
}

// awsAnswer is what aws prints at site, and the status it exits with.
func awsAnswer(site tlsSite) (string, int) {
	if site.control {
		return awsControlAnswer, 255
	}
	return fmt.Sprintf(awsTLSRefusal, site.url), 255
}

// The scripts the stand-in clients run (shTLSClient). sh runs each with the client's name as $0,
// the site's name as $1, a directory as $2, and what the client prints at the site and the status
// it exits with there as $3 and $4. A script that must be stopped runs exec sleep, so that the stop
// reaches the sleep itself, also where nothing stops a shell's children: the merge base's
// exec.CommandContext killed the shell alone.
const (
	// tlsAnswers prints $3, where there is any, on stderr and exits $4.
	tlsAnswers = `[ -z "$3" ] || printf '%s\n' "$3" >&2; exit "$4"`
	// tlsFirstRunSleeps sleeps past the limit on the client's first run of all, which is at
	// emSign, leaving a mark in $2, and answers on every later run.
	tlsFirstRunSleeps = `if [ ! -e "$2/$0.ran" ]; then : >"$2/$0.ran"; exec sleep 30; fi; ` + tlsAnswers
	// tlsSleepsAtEmSign sleeps past the limit on every run at emSign, and answers at every other
	// site.
	tlsSleepsAtEmSign = `[ "$1" != emSign ] || exec sleep 30; ` + tlsAnswers
)

// shTLSClient is a command client named name with a limit of one second whose run at a site is sh
// running script, as the constants above say, answer giving what it prints there and the status it
// exits with. Each run first appends "<name> <site>" to dir/runs.
func shTLSClient(name, dir, script string, answer func(tlsSite) (string, int)) tlsCommandClient {
	return tlsCommandClient{
		clientName: name,
		timeout:    time.Second,
		command: func(site tlsSite) (string, []string, []string) {
			output, status := answer(site)
			return "sh", []string{"-c", `printf '%s %s\n' "$0" "$1" >>"$2/runs"; ` + script, name, site.name, dir, output, strconv.Itoa(status)}, nil
		},
	}
}

// tlsLines are stdout's TLS lines of client at site, in the order they were printed.
func tlsLines(stdout, client, site string) []string {
	prefix := "probe-image: TLS client " + client + " " + site + ": "
	var lines []string
	for line := range strings.Lines(stdout) {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, strings.TrimSuffix(line, "\n"))
		}
	}
	return lines
}

// tlsRunsLogged are the runs the stand-in clients sharing dir logged, "<client> <site>" each, in
// the order they started.
func tlsRunsLogged(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// timedTLSClient is a client that sums the time its runs at each site took, by the site's name.
type timedTLSClient struct {
	tlsClient
	spent map[string]time.Duration
}

func (c timedTLSClient) probe(ctx context.Context, site tlsSite, terms tlsRunTerms) tlsOutcome {
	started := time.Now()
	defer func() { c.spent[site.name] += time.Since(started) }()
	return c.tlsClient.probe(ctx, site, terms)
}

// Every client of the image's inventory, of each kind (workerTLSClients: command, optional and
// Chromium clients), is slow on its first run at emSign, a site the positive controls reached: its
// own limit ends that run, which has no verdict, and its one more run refuses the chain in that
// client's own terms, so the probe passes. Each client is built as the probe builds it
// (tlsClientSpec.build), from a copy of its spec whose program is a stand-in named by an absolute
// path, so gh's and codegraph-node's own paths are replaced too, and whose limit is 300 ms. A
// client whose shell waits on a child holding the output pipe returns within its limit, the stop's
// grace and procgroup.WaitDelay, since its limit stops its whole process group. The positive
// controls' curl gets no second run: its own limit fails the probe.
func TestTLSProbeRunsEveryImageClientAgainAfterItsLimitAtAReachedSite(t *testing.T) {
	const limit = 300 * time.Millisecond
	stubs, stateDir := t.TempDir(), t.TempDir()
	// standIn writes an executable stand-in that runs firstRun on its first run of all, then exits
	// 0 at the control and prints refusal on stderr and exits 1 at every other site.
	standIn := func(name, firstRun, refusal string) string {
		path := filepath.Join(stubs, name)
		script := "#!/bin/sh\n" +
			"if [ ! -e '" + path + ".ran' ]; then : >'" + path + ".ran'; " + firstRun + "; fi\n" +
			"[ \"$LEGION_TLS_URL\" != '" + controlTLSSite.url + "' ] || exit 0\n" +
			"echo '" + refusal + "' >&2\nexit 1\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	refusalOf := func(spec tlsClientSpec) string {
		if spec.kind == tlsChromiumKind {
			return "Page load failed: net::ERR_CERT_AUTHORITY_INVALID"
		}
		return "certificate verify failed"
	}
	clients := make([]tlsClient, 0, len(workerTLSClients))
	for _, spec := range workerTLSClients {
		spec.program, spec.timeout = standIn(spec.name, "exec sleep 30", refusalOf(spec)), limit
		clients = append(clients, spec.build("omp", nil, stateDir))
	}

	observations, noVerdict, err := probeTLSClients(context.Background(), clients, compromisedTLSSites())

	if err != nil || len(noVerdict) != 0 {
		t.Fatalf("probeTLSClients() = %v, runs with no verdict %q; want every client to pass on its second run at emSign", err, noVerdict)
	}
	for _, spec := range workerTLSClients {
		var runs []tlsOutcome
		for _, observation := range observations {
			if observation.client == spec.name && observation.site.name == emSignTLSSite.name {
				runs = append(runs, observation.tlsOutcome)
			}
		}
		if len(runs) != 2 || runs[0].status != tlsNoVerdict || !strings.HasPrefix(runs[0].detail, "its 300ms limit ended it after ") || runs[1].status != tlsCertificateRefused {
			t.Errorf("%s at emSign ran %+v, want a first run its 300ms limit ended with no verdict, then a refusal", spec.name, runs)
		}
	}

	t.Run("a shell waiting on a child that holds the output pipe", func(t *testing.T) {
		spec := workerTLSClients[0]
		spec.program, spec.timeout = standIn("held", "sleep 30 & wait", refusalOf(spec)), limit
		started := time.Now()
		outcome := spec.build("omp", nil, stateDir).probe(context.Background(), emSignTLSSite, tlsRunTerms{reached: true})
		if elapsed, bound := time.Since(started), limit+procgroup.StopGrace+procgroup.WaitDelay; outcome.status != tlsNoVerdict || elapsed >= bound {
			t.Errorf("a run whose shell waits on a child holding its output = %+v after %s, want no verdict within %s", outcome, elapsed, bound)
		}
	})

	t.Run("the positive controls get no second run", func(t *testing.T) {
		path := filepath.Join(stubs, "positive-control")
		script := "#!/bin/sh\n" +
			"printf '%s\\n' \"$LEGION_TLS_URL\" >>'" + path + ".runs'\n" +
			"if [ ! -e '" + path + ".ran' ]; then : >'" + path + ".ran'; exec sleep 30; fi\n" +
			"[ \"$LEGION_TLS_URL\" != '" + isolationTLSSite.url + "' ] || { echo 'curl: (60) SSL certificate problem: unable to get local issuer certificate' >&2; exit 60; }\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		previous := tlsPositiveControlSpec
		t.Cleanup(func() { tlsPositiveControlSpec = previous })
		tlsPositiveControlSpec.program, tlsPositiveControlSpec.timeout = path, limit

		err := probeTLSPositiveControls(context.Background(), nil)

		if want := "emSign positive control at " + emSignTLSSite.url + " did not accept its pinned root: its 300ms limit ended it after "; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("probeTLSPositiveControls() = %v, want the failure %q", err, want)
		}
		raw, readErr := os.ReadFile(path + ".runs")
		if got := strings.Count(string(raw), emSignTLSSite.url+"\n"); readErr != nil || got != 1 {
			t.Errorf("the emSign positive control ran %d times (%v), want once: %q", got, readErr, raw)
		}
	})
}

// c-slow-probe-passes case (1): node, the first client, is slow on its first run at emSign, where
// only the positive control has reached the site. Its own limit ends that run with neither a
// refusal nor an answer in its output, so the run has no verdict, and its one more run, at two
// times the limit, refuses the chain: the probe passes. At the merge base the first run was
// unavailable and failed the probe ("node cannot verify refusal of the emSign chain: signal:
// killed"), as BET's and OSSV's probes failed on 2026-11-17.
func TestTLSProbePassesTheFirstClientWhoseSlowFirstRunAtEmSignRefusesOnItsSecondRun(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	node := shTLSClient("node", t.TempDir(), tlsFirstRunSleeps, nodeAnswer)

	code, stdout, stderr := probeImageWith(theseTLSClients(node), passingTLSPositiveControls, img.flags()...)

	if code != 0 || !strings.Contains(stdout, "\n"+bootprobe.OKPrefix+" (") {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 0 and the OK line", code, stdout, stderr)
	}
	lines := tlsLines(stdout, "node", emSignTLSSite.name)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "probe-image: TLS client node emSign: no verdict (its 1s limit ended it after ") || !strings.HasPrefix(lines[1], "probe-image: TLS client node emSign: refused (") {
		t.Errorf("node's emSign lines = %q, want a first run with no verdict naming its 1s limit, then a refusal", lines)
	}
}

// c-slow-probe-passes case (2): the same for aws, a later client, at emSign after node refused the
// chain there: aws's first run, which its own limit ends, has no verdict, and its second run
// refuses the chain, so the probe passes. At the merge base it failed the probe ("aws cannot
// verify refusal of the emSign chain: signal: killed").
func TestTLSProbePassesALaterClientWhoseSlowFirstRunAtEmSignRefusesOnItsSecondRun(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	dir := t.TempDir()
	node := shTLSClient("node", dir, tlsAnswers, nodeAnswer)
	aws := shTLSClient("aws", dir, tlsFirstRunSleeps, awsAnswer)
	aws.applicationSeen = awsResponseSeen

	code, stdout, stderr := probeImageWith(theseTLSClients(node, aws), passingTLSPositiveControls, img.flags()...)

	if code != 0 || !strings.Contains(stdout, "\n"+bootprobe.OKPrefix+" (") {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 0 and the OK line", code, stdout, stderr)
	}
	if lines := tlsLines(stdout, "node", emSignTLSSite.name); len(lines) != 1 || !strings.HasPrefix(lines[0], "probe-image: TLS client node emSign: refused (") {
		t.Errorf("node's emSign lines = %q, want one refusal", lines)
	}
	lines := tlsLines(stdout, "aws", emSignTLSSite.name)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "probe-image: TLS client aws emSign: no verdict (its 1s limit ended it after ") || !strings.HasPrefix(lines[1], "probe-image: TLS client aws emSign: refused (") {
		t.Errorf("aws's emSign lines = %q, want a first run with no verdict naming its 1s limit, then a refusal", lines)
	}
}

// c-slow-probe-passes case (3): a client whose every run at emSign, a reached site, outlasts its
// limit still has no verdict after its second run. Nothing failed, so `legion probe-image` exits
// bootprobe.NoVerdictExit with the line the daemon reads, naming the client and the site, and
// prints neither the OK line nor the failure's. The TLS run took its limit and two times its limit,
// and less than that plus what the two stops can add (StopGrace and WaitDelay each).
func TestTLSProbeExits75NamingAClientThatNeverAnswersAtAReachedSite(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	node := shTLSClient("node", t.TempDir(), tlsSleepsAtEmSign, nodeAnswer)
	timed := timedTLSClient{tlsClient: node, spent: map[string]time.Duration{}}

	code, stdout, stderr := probeImageWith(theseTLSClients(timed), passingTLSPositiveControls, img.flags()...)

	detail, ok := bootprobe.NoVerdict(stderr)
	if code != bootprobe.NoVerdictExit || !ok || !strings.HasPrefix(detail, "node emSign: its 2s limit ended it after ") || strings.Contains(detail, ";") {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit %d and the no-verdict line naming node's emSign run alone", code, stdout, stderr, bootprobe.NoVerdictExit)
	}
	if strings.Contains(stderr, "worker TLS probe failed") || strings.Contains(stdout, bootprobe.OKPrefix) {
		t.Errorf("probe-image printed the failure's line or the OK line: stdout %q, stderr %q", stdout, stderr)
	}
	least := 3 * node.timeout
	most := least + 2*(procgroup.StopGrace+procgroup.WaitDelay)
	if spent := timed.spent[emSignTLSSite.name]; spent < least || spent >= most {
		t.Errorf("node's emSign TLS run took %s, want at least %s and less than %s", spent, least, most)
	}
}

// c-slow-probe-passes case (4): a run its own limit ends at a site no earlier run reached fails the
// probe as before, since the network may not reach that site: here the first client's run at the
// control, which no positive control reaches. A compromised site no run reached is the same, and an
// earlier client's refusal there makes such a run one with no verdict. The run's own limit ended it
// also where the probe's deadline passes while the limit's stop is still under way, since the run
// records which of the two fired first when it fires: such a run is unavailable too, and never one
// with no verdict.
func TestTLSProbeFailsAClientWhoseLimitPassesAtASiteNoRunReached(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	node := shTLSClient("node", t.TempDir(), `[ "$1" != control ] || exec sleep 30; `+tlsAnswers, nodeAnswer)

	code, stdout, stderr := probeImageWith(theseTLSClients(node), passingTLSPositiveControls, img.flags()...)

	want := "network unavailable: node cannot reach " + controlTLSSite.url + ": its 1s limit ended it after "
	if code != 1 || !strings.Contains(stderr, want) || strings.Contains(stdout, bootprobe.OKPrefix) {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 1 naming %q", code, stdout, stderr, want)
	}
	if _, ok := bootprobe.NoVerdict(stderr); ok {
		t.Errorf("stderr %q carries the no-verdict line beside the failure", stderr)
	}
	if lines := tlsLines(stdout, "node", controlTLSSite.name); len(lines) != 1 {
		t.Errorf("node's control lines = %q, want one run and no second", lines)
	}

	dir := t.TempDir()
	aws := shTLSClient("aws", dir, tlsSleepsAtEmSign, awsAnswer)
	aws.applicationSeen = awsResponseSeen
	if _, noVerdict, err := probeTLSClients(context.Background(), []tlsClient{aws}, nil); err == nil || !strings.Contains(err.Error(), "aws cannot verify refusal of the emSign chain: its 1s limit ended it after ") || len(noVerdict) != 0 {
		t.Errorf("probeTLSClients() with emSign unreached = %v, runs with no verdict %q; want aws's emSign run to fail", err, noVerdict)
	}
	node = shTLSClient("node", dir, tlsAnswers, nodeAnswer)
	if _, noVerdict, err := probeTLSClients(context.Background(), []tlsClient{node, aws}, nil); err != nil || len(noVerdict) != 1 || !strings.HasPrefix(noVerdict[0], "aws emSign: its 2s limit ended it after ") {
		t.Errorf("probeTLSClients() with emSign reached by node's refusal = %v, runs with no verdict %q; want aws's emSign run alone with no verdict", err, noVerdict)
	}

	// A command at the control that answers its limit's SIGTERM, at 1 s, with an exit of its own
	// half a second later, while the probe's deadline passes at 1.2 s, in between.
	stopped := shTLSClient("node", t.TempDir(), `trap '/bin/sleep 0.5; exit 1' TERM; /bin/sleep 30 & wait`, nodeAnswer)
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	outcome := stopped.probe(ctx, controlTLSSite, tlsRunTerms{})
	if ctx.Err() == nil {
		t.Fatalf("the run returned %+v before the probe's deadline; the row needs the deadline to pass while the limit's stop is under way", outcome)
	}
	if outcome.status != tlsNetworkUnavailable || !strings.HasPrefix(outcome.detail, "its 1s limit ended it after ") || !strings.HasSuffix(outcome.detail, ": exit status 1") {
		t.Errorf("a run its 1s limit stopped, which exited after the probe's deadline = %+v, want unavailable naming its 1s limit", outcome)
	}
}

// c-slow-probe-passes case (5): the positive controls keep their rule, so a control its own limit
// ends fails the probe at once, with no second run: a pod with no egress fails here.
func TestTLSProbeFailsAPositiveControlThatPassesItsLimit(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	runs := filepath.Join(t.TempDir(), "runs")
	img.install(t, "curl", "#!/bin/sh\nprintf '%s\\n' \"$LEGION_TLS_URL\" >>'"+runs+"'\nexec sleep 30\n")
	previous := tlsPositiveControlSpec.timeout
	t.Cleanup(func() { tlsPositiveControlSpec.timeout = previous })
	tlsPositiveControlSpec.timeout = time.Second

	code, stdout, stderr := probeImageWith(passingImageTLSClients, probeTLSPositiveControls, img.flags()...)

	want := "worker TLS probe cannot verify test chains: emSign positive control at " + emSignTLSSite.url + " did not accept its pinned root: its 1s limit ended it after "
	if code != 1 || !strings.Contains(stderr, want) || strings.Contains(stdout, bootprobe.OKPrefix) || strings.Contains(stdout, "probe-image: TLS client ") {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 1 naming %q before any client runs", code, stdout, stderr, want)
	}
	if _, ok := bootprobe.NoVerdict(stderr); ok {
		t.Errorf("stderr %q carries the no-verdict line beside the failure", stderr)
	}
	raw, err := os.ReadFile(runs)
	if got := strings.Count(string(raw), emSignTLSSite.url+"\n"); err != nil || got != 1 {
		t.Errorf("the emSign positive control ran %d times (%v), want once: %q", got, err, raw)
	}
}

// c-slow-probe-passes case (7): a failure fails the probe beside a TLS run with no verdict, and
// decides it: a client that accepts the emSign chain on its second run, one that exits at emSign
// with no certificate error before its limit, one a signal kills before its limit, one a signal
// kills before its limit after it printed a certificate refusal, which the signal leaves
// unavailable all the same, and one that prints `signal: killed` itself and exits 1, which is read
// from the run's own record as an exit and never as a signal or a limit. Beside each, bun's runs at
// emSign have no verdict, and no run exits 75.
func TestTLSProbeFailsWhenAFailureStandsBesideARunWithNoVerdict(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	nodeAtEmSign := func(output string, status int) func(tlsSite) (string, int) {
		return func(site tlsSite) (string, int) {
			if site.name == emSignTLSSite.name {
				return output, status
			}
			return nodeAnswer(site)
		}
	}
	for _, tc := range []struct {
		name, client, script string
		answer               func(tlsSite) (string, int)
		want                 string
		line                 *regexp.Regexp
	}{
		{name: "accepts emSign", client: "aws", script: tlsFirstRunSleeps,
			answer: func(tlsSite) (string, int) { return awsControlAnswer, 255 },
			want:   "aws accepts the emSign chain: "},
		{name: "exits at emSign with no certificate error", client: "node", script: tlsAnswers,
			answer: nodeAtEmSign("TypeError: fetch failed [cause]: read ECONNRESET", 1),
			want:   "node cannot verify refusal of the emSign chain: exit status 1: TypeError: fetch failed"},
		{name: "killed by a signal", client: "node", script: `[ "$1" != emSign ] || kill -9 $$; ` + tlsAnswers, answer: nodeAnswer,
			want: "node cannot verify refusal of the emSign chain: signal: killed ended it after ",
			line: regexp.MustCompile(`^probe-image: TLS client node emSign: unavailable \(signal: killed ended it after [0-9]+\.[0-9]s, before its 1s limit: signal: killed\)$`)},
		{name: "killed by a signal after it printed a certificate refusal", client: "node", script: `if [ "$1" = emSign ]; then printf '%s\n' "$3" >&2; kill -9 $$; fi; ` + tlsAnswers, answer: nodeAnswer,
			want: "node cannot verify refusal of the emSign chain: signal: killed ended it after ",
			line: regexp.MustCompile(`^probe-image: TLS client node emSign: unavailable \(signal: killed ended it after [0-9]+\.[0-9]s, before its 1s limit: signal: killed: node:internal/.*CERT_REVOKED.*\)$`)},
		{name: "prints signal: killed itself and exits 1", client: "node", script: tlsAnswers, answer: nodeAtEmSign("signal: killed", 1),
			want: "node cannot verify refusal of the emSign chain: exit status 1: signal: killed",
			line: regexp.MustCompile(`^probe-image: TLS client node emSign: unavailable \(exit status 1: signal: killed\)$`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bun := shTLSClient("bun", dir, tlsSleepsAtEmSign, nodeAnswer)
			failing := shTLSClient(tc.client, dir, tc.script, tc.answer)
			if tc.client == "aws" {
				failing.applicationSeen = awsResponseSeen
			}

			code, stdout, stderr := probeImageWith(theseTLSClients(bun, failing), passingTLSPositiveControls, img.flags()...)

			if code != 1 || !strings.Contains(stderr, "legion probe-image: worker TLS probe failed: ") || !strings.Contains(stderr, tc.want) || strings.Contains(stdout, bootprobe.OKPrefix) {
				t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 1 naming %q", code, stdout, stderr, tc.want)
			}
			if detail, ok := bootprobe.NoVerdict(stderr); ok {
				t.Errorf("stderr carries the no-verdict line %q beside the failure", detail)
			}
			if lines := tlsLines(stdout, "bun", emSignTLSSite.name); len(lines) != 2 || !strings.HasPrefix(lines[0], "probe-image: TLS client bun emSign: no verdict (") || !strings.HasPrefix(lines[1], "probe-image: TLS client bun emSign: no verdict (") {
				t.Errorf("bun's emSign lines = %q, want two runs with no verdict", lines)
			}
			if tc.line != nil && !slices.ContainsFunc(tlsLines(stdout, tc.client, emSignTLSSite.name), tc.line.MatchString) {
				t.Errorf("%s's emSign lines = %q, want one matching %s", tc.client, tlsLines(stdout, tc.client, emSignTLSSite.name), tc.line)
			}
		})
	}
}

// c-slow-probe-passes case (8): a run its own limit ends whose output already shows a certificate
// error is a refusal, decided by that output: its line says refused and names the limit, and it
// gets no second run.
func TestTLSProbeReadsAKilledRunThatPrintedACertificateErrorAsARefusal(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	node := shTLSClient("node", t.TempDir(), `if [ "$1" != control ]; then printf '%s\n' "$3" >&2; exec sleep 30; fi; `+tlsAnswers, nodeAnswer)

	code, stdout, stderr := probeImageWith(theseTLSClients(node), passingTLSPositiveControls, img.flags()...)

	if code != 0 || !strings.Contains(stdout, "\n"+bootprobe.OKPrefix+" (") {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 0 and the OK line", code, stdout, stderr)
	}
	for _, site := range compromisedTLSSites() {
		lines := tlsLines(stdout, "node", site.name)
		if len(lines) != 1 || !strings.HasPrefix(lines[0], "probe-image: TLS client node "+site.name+": refused (its 1s limit ended it after ") || !strings.Contains(lines[0], "CERT_REVOKED") {
			t.Errorf("node's %s lines = %q, want one refusal naming its 1s limit and the certificate error", site.name, lines)
		}
	}
}

// The state table's row of a run its own limit ends whose output already shows an answer: node,
// first, reaches the control, and aws, whose control run prints the page it could not parse and is
// then slow, is accepted there, decided by that output: its line names the limit, and it gets no
// second run. node cannot carry this row: its answer at the control is an exit 0 with no output,
// and its client reads no answer marker, so a killed node run shows no answer.
func TestTLSProbeReadsAKilledRunThatPrintedAnAnswerAsAccepted(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	dir := t.TempDir()
	node := shTLSClient("node", dir, tlsAnswers, nodeAnswer)
	aws := shTLSClient("aws", dir, `if [ "$1" = control ]; then printf '%s\n' "$3" >&2; exec sleep 30; fi; `+tlsAnswers, awsAnswer)
	aws.applicationSeen = awsResponseSeen

	code, stdout, stderr := probeImageWith(theseTLSClients(node, aws), passingTLSPositiveControls, img.flags()...)

	if code != 0 || !strings.Contains(stdout, "\n"+bootprobe.OKPrefix+" (") {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 0 and the OK line", code, stdout, stderr)
	}
	if lines := tlsLines(stdout, "aws", controlTLSSite.name); len(lines) != 1 || !strings.HasPrefix(lines[0], "probe-image: TLS client aws control: accepted (its 1s limit ended it after ") || !strings.Contains(lines[0], "invalid XML received") {
		t.Errorf("aws's control lines = %q, want one acceptance naming its 1s limit and the page", lines)
	}
}

// c-slow-probe-passes case (9): the TLS line of a run its own limit ended says so, with the limit,
// the second run's two times the first, and how long the run took; the line of a run a signal
// ended before its limit names the signal; a run that exited on its own keeps the detail it always
// had. Each detail then carries what the run printed and its error.
func TestTLSProbeLineNamesWhatEndedARunAndAfterHowLong(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	dir := t.TempDir()
	node := shTLSClient("node", dir, tlsSleepsAtEmSign, nodeAnswer)
	aws := shTLSClient("aws", dir, `[ "$1" != emSign ] || kill -9 $$; `+tlsAnswers, awsAnswer)
	aws.applicationSeen = awsResponseSeen

	_, stdout, stderr := probeImageWith(theseTLSClients(node, aws), passingTLSPositiveControls, img.flags()...)

	seconds := func(line string, form *regexp.Regexp) (float64, bool) {
		match := form.FindStringSubmatch(line)
		if match == nil {
			return 0, false
		}
		elapsed, err := strconv.ParseFloat(match[1], 64)
		return elapsed, err == nil
	}
	bound := (procgroup.StopGrace + procgroup.WaitDelay).Seconds()
	emSign := tlsLines(stdout, "node", emSignTLSSite.name)
	if len(emSign) != 2 {
		t.Fatalf("node's emSign lines = %q, want two; stdout %q, stderr %q", emSign, stdout, stderr)
	}
	for i, limit := range []float64{1, 2} {
		form := regexp.MustCompile(`^probe-image: TLS client node emSign: no verdict \(its ` + strconv.FormatFloat(limit, 'f', -1, 64) + `s limit ended it after ([0-9]+\.[0-9])s: signal: terminated\)$`)
		if elapsed, ok := seconds(emSign[i], form); !ok || elapsed < limit || elapsed >= limit+bound {
			t.Errorf("node's emSign line %d = %q, want it to name its %gs limit and a time from %gs to less than %gs", i+1, emSign[i], limit, limit, limit+bound)
		}
	}
	signal := regexp.MustCompile(`^probe-image: TLS client aws emSign: unavailable \(signal: killed ended it after ([0-9]+\.[0-9])s, before its 1s limit: signal: killed\)$`)
	if lines := tlsLines(stdout, "aws", emSignTLSSite.name); len(lines) != 1 {
		t.Errorf("aws's emSign lines = %q, want one", lines)
	} else if elapsed, ok := seconds(lines[0], signal); !ok || elapsed >= 1 {
		t.Errorf("aws's emSign line = %q, want it to name the signal and a time under its 1s limit", lines[0])
	}
	if lines := tlsLines(stdout, "node", aaaTLSSite.name); len(lines) != 1 || lines[0] != "probe-image: TLS client node AAA: refused ("+trimTLSDetail("exit status 1: "+nodeTLSRefusal)+")" {
		t.Errorf("node's AAA lines = %q, want the refusal's detail as a run that exited has always had it", lines)
	}
}

// One more run comes after every client's first run at every site, in the same order, with two
// times the client's limit: the runs' log shows aws's second run at emSign after the last client's
// control run, and its line names a 2s limit where the first named 1s. A run the probe's deadline
// ended gets no second run, nor does any run after it, which never starts.
func TestTLSProbeRunsARunWithNoVerdictAgainAfterEveryOtherRunAtTwiceItsLimit(t *testing.T) {
	dir := t.TempDir()
	aws := shTLSClient("aws", dir, tlsSleepsAtEmSign, awsAnswer)
	aws.applicationSeen = awsResponseSeen
	node := shTLSClient("node", dir, tlsAnswers, nodeAnswer)

	observations, noVerdict, err := probeTLSClients(context.Background(), []tlsClient{aws, node}, compromisedTLSSites())

	want := []string{"aws emSign", "aws AAA", "aws control", "node emSign", "node AAA", "node control", "aws emSign"}
	if got := tlsRunsLogged(t, dir); !slices.Equal(got, want) {
		t.Errorf("the clients ran %q, want %q", got, want)
	}
	if len(observations) != len(want) {
		t.Fatalf("probeTLSClients() observed %d runs, want %d: %+v", len(observations), len(want), observations)
	}
	first, second := observations[0], observations[len(observations)-1]
	if first.client != "aws" || first.site.name != emSignTLSSite.name || first.status != tlsNoVerdict || !strings.HasPrefix(first.detail, "its 1s limit ended it after ") {
		t.Errorf("first observation = %+v, want aws's emSign run with no verdict, its 1s limit named", first)
	}
	if second.client != "aws" || second.site.name != emSignTLSSite.name || second.status != tlsNoVerdict || !strings.HasPrefix(second.detail, "its 2s limit ended it after ") {
		t.Errorf("last observation = %+v, want aws's second emSign run with no verdict, its 2s limit named", second)
	}
	if err != nil || len(noVerdict) != 1 || !strings.HasPrefix(noVerdict[0], "aws emSign: its 2s limit ended it after ") {
		t.Errorf("probeTLSClients() = %v, runs with no verdict %q; want no failure and aws's emSign run, as its second run left it", err, noVerdict)
	}

	t.Run("a run the probe's deadline ended", func(t *testing.T) {
		dir := t.TempDir()
		aws := shTLSClient("aws", dir, tlsSleepsAtEmSign, awsAnswer)
		node := shTLSClient("node", dir, tlsAnswers, nodeAnswer)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()

		observations, noVerdict, err := probeTLSClients(ctx, []tlsClient{aws, node}, compromisedTLSSites())

		if got := tlsRunsLogged(t, dir); !slices.Equal(got, []string{"aws emSign"}) {
			t.Errorf("the clients ran %q, want aws's emSign run alone", got)
		}
		if len(observations) != 2*len(tlsSites) || len(noVerdict) != len(observations) || err != nil {
			t.Fatalf("probeTLSClients() = %v, %d observations, runs with no verdict %q; want one run of each client at each site, each with no verdict", err, len(observations), noVerdict)
		}
		for _, observation := range observations {
			if observation.status != tlsNoVerdict || !strings.HasPrefix(observation.detail, "the probe's deadline ended it after ") {
				t.Errorf("%s at %s = %+v, want no verdict, the probe's deadline named", observation.client, observation.site.name, observation.tlsOutcome)
			}
		}
	})
}

// A failure seen before the probe's deadline stands, also inside a step that returns after it: the
// emSign positive control is refused at once, and the AAA control, slow, is cut by the deadline,
// which leaves that run with no verdict and the emSign failure deciding the probe.
func TestTLSProbeKeepsAFailureSeenBeforeTheDeadline(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	img.install(t, "curl", "#!/bin/sh\n[ \"$LEGION_TLS_URL\" != '"+aaaTLSSite.url+"' ] || exec sleep 30\necho 'curl: (60) SSL certificate problem: unable to get local issuer certificate' >&2\nexit 60\n")
	previous := tlsPositiveControlSpec.timeout
	t.Cleanup(func() { tlsPositiveControlSpec.timeout = previous })
	tlsPositiveControlSpec.timeout = time.Minute

	code, stdout, stderr := probeImageWith(passingImageTLSClients, probeTLSPositiveControls, img.flags("--probe-budget", shortProbeBudget)...)

	want := "emSign positive control at " + emSignTLSSite.url + " did not accept its pinned root: exit status 60: curl: (60) SSL certificate problem"
	if code != 1 || !strings.Contains(stderr, want) || strings.Contains(stdout, bootprobe.OKPrefix) {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 1 naming %q", code, stdout, stderr, want)
	}
	if strings.Contains(stderr, "AAA positive control at "+aaaTLSSite.url+" did not accept") {
		t.Errorf("stderr %q names the AAA control the deadline cut as a failure", stderr)
	}
	if _, ok := bootprobe.NoVerdict(stderr); ok {
		t.Errorf("stderr %q carries the no-verdict line beside the failure", stderr)
	}
}

// The third way a site is reached: node, first, is accepted at the control, so bun's first control
// run, which its own limit ends with no answer in its output, has no verdict where it would fail
// with no earlier run there, and bun's second run, which answers, is accepted.
func TestTLSProbeRunsAgainALaterClientCutByItsLimitAtAControlAnEarlierClientReached(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	dir := t.TempDir()
	node := shTLSClient("node", dir, tlsAnswers, nodeAnswer)
	bun := shTLSClient("bun", dir, `if [ "$1" = control ] && [ ! -e "$2/$0.ran" ]; then : >"$2/$0.ran"; exec sleep 30; fi; `+tlsAnswers, nodeAnswer)

	code, stdout, stderr := probeImageWith(theseTLSClients(node, bun), passingTLSPositiveControls, img.flags()...)

	if code != 0 || !strings.Contains(stdout, "\n"+bootprobe.OKPrefix+" (") {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 0 and the OK line", code, stdout, stderr)
	}
	lines := tlsLines(stdout, "bun", controlTLSSite.name)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "probe-image: TLS client bun control: no verdict (its 1s limit ended it after ") || !strings.HasPrefix(lines[1], "probe-image: TLS client bun control: accepted (") {
		t.Errorf("bun's control lines = %q, want a first run with no verdict naming its 1s limit, then an acceptance", lines)
	}
}
