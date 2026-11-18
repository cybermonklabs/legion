package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

type fakeTLSClient struct {
	clientName string
	outcomes   map[string]tlsOutcome
}

func (c fakeTLSClient) name() string { return c.clientName }

func (c fakeTLSClient) probe(_ context.Context, site tlsSite) tlsOutcome {
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
			_, err := probeTLSClients(context.Background(), []tlsClient{fakeTLSClient{clientName: "node", outcomes: testCase.outcomes}})
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

// curl names one refusal in two ways, by version, and an image may carry either: an image built
// on another distribution keeps that distribution's curl. Read as no certificate error, a positive
// control's refusal is "unavailable" and the probe fails an image that is safe.
func TestTLSCertificateErrorRecognizesEachCurlRefusal(t *testing.T) {
	for _, testCase := range []struct {
		name, detail string
		want         bool
	}{
		{"curl 8.14", "curl: (60) SSL certificate problem: unable to get local issuer certificate", true},
		{"curl 8.18", "curl: (60) SSL certificate OpenSSL verify result: unable to get local issuer certificate (20)", true},
		{"no route", "curl: (7) Failed to connect to example.com port 443 after 3 ms: Could not connect to server", false},
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

	outcome := client.probe(context.Background(), controlTLSSite)

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
// whose chain it accepted. uv's three lines were measured on 2026-11-18 on the image of 841974e9
// (sha256:37018eaa…, uv 0.13.5) in `legion probe-image`: an image whose /etc/uv/uv.toml allows
// the emSign and AAA hosts made uv accept both chains, and the probe failed naming uv as unable
// to verify a refusal rather than as accepting the chain, because the site's text/plain page is
// not an answer uvResponseSeen read.
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
		{client: "uv", name: "control", exit: "1", stderr: "Using Python 3.13.5 environment at: /usr\nerror: No solution found when resolving dependencies:\n  ╰─▶ Because legion-tls-probe-not-a-package was not found in the package registry and you require legion-tls-probe-not-a-package, we can conclude that your requirements are unsatisfiable.", want: tlsConnectionAccepted},
		{client: "uv", name: "refusal", exit: "2", stderr: "Using Python 3.13.5 environment at: /usr\nerror: Failed to fetch: `https://dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe-not-a-package/`\n  Caused by: client error (Connect)\n  Caused by: invalid peer certificate: Other(OtherError(CertDistrusted))", want: tlsCertificateRefused},
		{client: "uv", name: "accepting a compromised chain", exit: "1", stderr: "Using Python 3.13.5 environment at: /usr\nerror: Unsupported `Content-Type` \"text/plain\" for `https://dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe-not-a-package/`. Expected JSON or HTML.", want: tlsConnectionAccepted},
	} {
		t.Run(testCase.client+" "+testCase.name, func(t *testing.T) {
			client := clients[testCase.client]
			client.command = func(tlsSite) (string, []string, []string) {
				return "sh", []string{"-c", `printf %s "$1" >&2; exit "$2"`, "sh", testCase.stderr, testCase.exit}, nil
			}

			if outcome := client.probe(context.Background(), controlTLSSite); outcome.status != testCase.want {
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

			_, err := probeTLSClients(context.Background(), standIns(append(slices.Clone(pod), testCase.setting)))
			for _, name := range testCase.clients {
				if want := name + " accepts the emSign chain"; err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("probeTLSClients() under a pod env with %s = %v, want an error containing %q", testCase.setting, err, want)
				}
			}
			if _, err := probeTLSClients(context.Background(), standIns(pod)); err != nil {
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

			if outcome := client.probe(context.Background(), controlTLSSite); outcome.status != testCase.want {
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
		observations, err := probeTLSClients(context.Background(), []tlsClient{acceptingEverySite(program)})
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

	if _, err := probeTLSClients(context.Background(), []tlsClient{acceptingEverySite("sh")}); err == nil || !strings.Contains(err.Error(), "optional accepts the emSign chain") {
		t.Fatalf("probeTLSClients() with the program on PATH accepting every chain = %v, want the emSign acceptance named", err)
	}

	optional := imageTLSClientsOf[tlsOptionalClient](t, nil, t.TempDir(), optionalTLSClientNames...)
	t.Setenv("PATH", bin)
	for name, client := range optional {
		for _, site := range tlsSites {
			if outcome := client.probe(context.Background(), site); outcome.status != tlsClientAbsent || outcome.detail != name+" is not on PATH" {
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

func TestTLSPositiveControlsRequireTheExpectedChain(t *testing.T) {
	for _, testCase := range []struct {
		name, want         string
		outcome, isolation tlsOutcome
	}{
		{name: "accepts pinned root", outcome: tlsOutcome{status: tlsConnectionAccepted}, isolation: tlsOutcome{status: tlsCertificateRefused}},
		{name: "refuses pinned root", outcome: tlsOutcome{status: tlsCertificateRefused, detail: "certificate verify failed"}, isolation: tlsOutcome{status: tlsCertificateRefused}, want: "emSign positive control"},
		{name: "cannot reach site", outcome: tlsOutcome{status: tlsNetworkUnavailable, detail: "network is unreachable"}, isolation: tlsOutcome{status: tlsCertificateRefused}, want: "emSign positive control"},
		{name: "cannot reach isolation endpoint", outcome: tlsOutcome{status: tlsConnectionAccepted}, isolation: tlsOutcome{status: tlsNetworkUnavailable, detail: "network is unreachable"}, want: "isolation endpoint"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := verifyTLSPositiveControls([]tlsSite{emSignTLSSite}, func(site tlsSite) tlsOutcome {
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

func TestTLSCommandPastTheProbeDeadlineIsUnavailableAndFailsTheProbe(t *testing.T) {
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
			return "sh", []string{"-c", "exec sleep 1"}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	observations, err := probeTLSClients(ctx, []tlsClient{client})
	if len(observations) != 2 {
		t.Fatalf("probeTLSClients() returned %d observations, want the client to reach both sites", len(observations))
	}
	if got := observations[1]; got.status != tlsNetworkUnavailable || got.detail != "probe deadline" {
		t.Fatalf("AAA outcome = %+v, want unavailable (probe deadline)", got.tlsOutcome)
	}
	if err == nil || !strings.Contains(err.Error(), "curl cannot verify refusal of the AAA chain: probe deadline") {
		t.Fatalf("probeTLSClients() = %v, want the deadline to name curl and AAA", err)
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

			if outcome := client.probe(context.Background(), controlTLSSite); outcome.status != testCase.want {
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

			outcome := client.probe(context.Background(), controlTLSSite)
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

	outcome := chromium.probe(context.Background(), controlTLSSite)
	if outcome.status != tlsConnectionAccepted {
		t.Fatalf("Chromium probe after a 21s cold start = %+v, want accepted", outcome)
	}
}
