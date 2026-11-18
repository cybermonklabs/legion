package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeTLSClient struct {
	clientName string
	outcomes   map[string]tlsOutcome
}

func (c fakeTLSClient) name() string { return c.clientName }

func (c fakeTLSClient) probe(_ context.Context, site tlsSite) tlsOutcome {
	return c.outcomes[site.name]
}

func passingImageTLSClients(string, []string, string, string) []tlsClient {
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
)

// GNU wget and git's https transport verify through GnuTLS, and each names a refusal in words of
// its own. Neither control answer is a refusal: git's is a failure, but one the site answered.
func TestTLSCertificateErrorRecognizesWgetAndGitRefusals(t *testing.T) {
	for _, testCase := range []struct {
		name, detail string
		want         bool
	}{
		{"wget refusal", wgetEmSignRefusal, true},
		{"wget control", wgetControlAnswer, false},
		{"git refusal", gitEmSignRefusal, true},
		{"git control", gitControlAnswer, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := tlsCertificateError(testCase.detail); got != testCase.want {
				t.Fatalf("tlsCertificateError(%q) = %v, want %v", testCase.detail, got, testCase.want)
			}
		})
	}
}

// git and go fail at the control too, since example.com serves neither a git repository nor a Go
// module path, but each names the site's answer, after a handshake it accepted. So does each at a
// compromised site once it has accepted that chain, which the probe must report as accepted and
// never as unavailable. A refusal names no answer.
func TestGitAndGoResponseSeenReadTheSitesAnswer(t *testing.T) {
	for _, testCase := range []struct {
		name, detail string
		seen         func(string) bool
		want         bool
	}{
		{"git control", gitControlAnswer, gitResponseSeen, true},
		{"git refusal", gitEmSignRefusal, gitResponseSeen, false},
		{"git accepting a compromised chain", gitEmSignAccepted, gitResponseSeen, true},
		{"go control", goControlAnswer, goResponseSeen, true},
		{"go refusal", goEmSignRefusal, goResponseSeen, false},
		{"go accepting a compromised chain", goEmSignAccepted, goResponseSeen, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.seen(testCase.detail); got != testCase.want {
				t.Fatalf("response seen in %q = %v, want %v", testCase.detail, got, testCase.want)
			}
		})
	}
}

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
	clients := imageTLSClients("omp", nil, "", "")
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

// workerTLSClients calls itself the complete TLS client inventory of the worker image. GNU wget
// (the `wget` the capability check's toolchain row requires on PATH, packages/daemon/internal/
// capabilities/image.go) and git (the PATH's git, which fetches over https with its own TLS
// library) are TLS clients on that PATH, so each must be probed against both refused chains and
// the control like every other client. wget is on the floor every deployment's image is held to,
// so its client must not be one an image may lack and still pass.
func TestWorkerTLSInventoryHoldsGNUWgetAndGit(t *testing.T) {
	clients := map[string]tlsClient{}
	for _, client := range imageTLSClients("omp", nil, "", "") {
		clients[client.name()] = client
	}
	for _, name := range []string{"wget", "git"} {
		if _, ok := clients[name]; !ok {
			t.Errorf("the TLS client inventory %q holds no %s client, although %s is a TLS client on the image's PATH", workerTLSClientNames(), name, name)
		}
	}
	if _, optional := clients["wget"].(tlsOptionalClient); optional {
		t.Errorf("the wget TLS client is optional, but the toolchain capability row requires wget on PATH")
	}
}

// redis-cli is a TLS client on the floor: the capability check's toolchain row requires it on PATH
// (packages/daemon/internal/capabilities/image.go), and `redis-cli --tls` reaches any host and port
// a role names, verifying its peer against OpenSSL's default store. It reaches a verdict at each
// site, measured on 2026-11-18 on the pull request's image (sha256:25d2edfc…, redis-cli 8.0.2) as
// `redis-cli --tls --sni <host> -h <host> -p 443 PING`: the emSign and AAA sites refuse at the
// handshake, example.com answers past it with HTTP, and with --insecure the compromised sites
// close the connection only after the handshake. So, as wget, go and kubectl, it is a floor client
// the inventory holds, and an answer past the handshake at a compromised site reads as accepting
// the chain rather than as unavailable.
func TestWorkerTLSInventoryHoldsRedisCLI(t *testing.T) {
	var client tlsCommandClient
	found := false
	for _, candidate := range imageTLSClients("omp", nil, "", t.TempDir()) {
		if candidate.name() != "redis-cli" {
			continue
		}
		command, ok := candidate.(tlsCommandClient)
		if !ok {
			t.Fatalf("the redis-cli TLS client is a %T, want a floor command client: the toolchain capability row requires redis-cli on PATH", candidate)
		}
		client, found = command, true
	}
	if !found {
		t.Fatalf("the TLS client inventory %q holds no redis-cli client, although redis-cli is a TLS client on the floor's toolchain row", workerTLSClientNames())
	}
	for _, testCase := range []struct {
		name, stderr string
		want         tlsStatus
	}{
		{"refusal", "Could not connect to Redis at dv-g3b-emudhra.we-have-a-problem.com:443: SSL_connect failed: certificate verify failed", tlsCertificateRefused},
		{"control", `Error: Protocol error, got "H" as reply type byte`, tlsConnectionAccepted},
		{"accepting a compromised chain", "Error: Server closed the connection", tlsConnectionAccepted},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			probe := client
			probe.command = func(tlsSite) (string, []string, []string) {
				return "sh", []string{"-c", `printf %s "$1" >&2; exit 1`, "sh", testCase.stderr}, nil
			}
			if outcome := probe.probe(context.Background(), emSignTLSSite); outcome.status != testCase.want {
				t.Fatalf("redis-cli probe of %q = %+v, want status %q", testCase.stderr, outcome, testCase.want)
			}
		})
	}
}

// imageFloorTLSClients returns the image's command clients that are not optional, by name, each of
// wget, git, go, kubectl and redis-cli present.
func imageFloorTLSClients(t *testing.T, workDir, stateDir string) map[string]tlsCommandClient {
	t.Helper()
	clients := map[string]tlsCommandClient{}
	for _, client := range imageTLSClients("omp", nil, workDir, stateDir) {
		if command, ok := client.(tlsCommandClient); ok {
			clients[command.name()] = command
		}
	}
	for _, name := range []string{"wget", "git", "go", "kubectl", "redis-cli"} {
		if _, ok := clients[name]; !ok {
			t.Fatalf("imageTLSClients() has no %s command client", name)
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

// wget, git, go and kubectl are each classified from the output and exit status their program
// gave: the control's answer is an acceptance and the emSign site's refusal a refusal. git and go
// fail at the control as well, so each needs the response marker its client reads, and the same
// marker reads the answer each gives at a compromised site whose chain it accepted.
func TestImageFloorTLSClientsClassifyTheirMeasuredOutput(t *testing.T) {
	clients := imageFloorTLSClients(t, "", t.TempDir())
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

// go runs in the probe's state directory, so no go.mod of the probe's working directory is read,
// and every path it writes is under that directory; it fetches the module path from the site's
// host itself and never downloads a toolchain. kubectl keeps its home and configuration there too,
// and with the in-cluster variables empty client-go never takes the in-cluster configuration,
// which would send the pod's service-account token to a test site. The pod's values below are
// ones each client's own environment must override.
func TestImageGoAndKubectlTLSClientsKeepToTheStateDirectory(t *testing.T) {
	stateDir, remove, err := newTLSStateDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
	clients := imageFloorTLSClients(t, t.TempDir(), stateDir)
	goClient, kubectl := clients["go"], clients["kubectl"]
	inStateDir := func(path string) bool { return strings.HasPrefix(path, stateDir+string(os.PathSeparator)) }
	pod := []string{
		"HOME=/home/legion", "XDG_CONFIG_HOME=/home/legion/.config", "GOPATH=/home/legion/go",
		"GOMODCACHE=/legion/go/mod", "GOCACHE=/legion/go/build", "GOPROXY=https://proxy.golang.org",
		"GOTOOLCHAIN=auto", "GOFLAGS=-mod=mod", "GOWORK=/legion/go.work", "KUBECONFIG=/home/legion/.kube/config",
		"KUBERNETES_SERVICE_HOST=192.0.2.1", "KUBERNETES_SERVICE_PORT=443",
	}

	if goClient.workDir != stateDir {
		t.Errorf("go runs in %q, want the state directory %s", goClient.workDir, stateDir)
	}
	for site, host := range tlsSiteHosts {
		program, args, extraEnv := goClient.command(site)
		if want := []string{"mod", "download", host + "/legion-tls-probe@latest"}; program != "go" || !slices.Equal(args, want) {
			t.Errorf("go command for %s = %s %q, want go %q", site.name, program, args, want)
		}
		env := tlsEnvironment(pod, extraEnv)
		for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "GOPATH", "GOMODCACHE", "GOCACHE"} {
			if value := envValue(env, name); !inStateDir(value) {
				t.Errorf("go at %s has %s %q, want a path under %s", site.name, name, value, stateDir)
			}
		}
		for name, want := range map[string]string{"GOPROXY": "direct", "GOTOOLCHAIN": "local", "GOFLAGS": "", "GOWORK": "off"} {
			if got := envValue(env, name); got != want {
				t.Errorf("go at %s has %s=%q, want %q", site.name, name, got, want)
			}
		}

		program, args, extraEnv = kubectl.command(site)
		if want := []string{"--server=" + site.url, "--token=" + tlsProbeSignIn, "get", "--raw", "/"}; program != "kubectl" || !slices.Equal(args, want) {
			t.Errorf("kubectl command for %s = %s %q, want kubectl %q", site.name, program, args, want)
		}
		env = tlsEnvironment(pod, extraEnv)
		for _, name := range []string{"HOME", "KUBECONFIG"} {
			if value := envValue(env, name); !inStateDir(value) {
				t.Errorf("kubectl at %s has %s %q, want a path under %s", site.name, name, value, stateDir)
			}
		}
		for _, name := range []string{"KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT"} {
			if got := envValue(env, name); got != "" {
				t.Errorf("kubectl at %s has %s=%q, want it empty so client-go never takes the in-cluster configuration", site.name, name, got)
			}
		}
	}
}

// redis-cli connects to the site's host, names that host in its SNI, and uses the https port,
// since no site's URL names another; it sends PING, which no site answers as Redis. It sends
// REDISCLI_AUTH as AUTH to the server it reaches, here a test site, so the pod's REDISCLI_AUTH must
// not reach redis-cli; every other variable of the pod's does.
func TestImageRedisCLITLSClientTargetsTheSitesHost(t *testing.T) {
	client := imageFloorTLSClients(t, "", t.TempDir())["redis-cli"]
	pod := []string{"HOME=/home/legion", "REDISCLI_AUTH=pod-credential", "PATH=/usr/bin"}
	for site, host := range tlsSiteHosts {
		program, args, extraEnv := client.command(site)
		if want := []string{"--tls", "--sni", host, "-h", host, "-p", "443", "PING"}; program != "redis-cli" || !slices.Equal(args, want) {
			t.Errorf("redis-cli command for %s = %s %q, want redis-cli %q", site.name, program, args, want)
		}
		if env, want := tlsEnvironment(pod, extraEnv), []string{"HOME=/home/legion", "PATH=/usr/bin"}; !slices.Equal(env, want) {
			t.Errorf("redis-cli environment at %s = %q, want %q: REDISCLI_AUTH dropped and nothing else", site.name, env, want)
		}
	}
}

// imageOptionalTLSClients returns the image's optional clients by name, each of gcloud, bq, az,
// mise and socat present.
func imageOptionalTLSClients(t *testing.T, stateDir string) map[string]tlsOptionalClient {
	t.Helper()
	clients := map[string]tlsOptionalClient{}
	for _, client := range imageTLSClients("omp", nil, "", stateDir) {
		if optional, ok := client.(tlsOptionalClient); ok {
			clients[optional.name()] = optional
		}
	}
	for _, name := range []string{"gcloud", "bq", "az", "mise", "socat"} {
		if _, ok := clients[name]; !ok {
			t.Fatalf("imageTLSClients() has no optional %s client", name)
		}
	}
	return clients
}

// Each optional client is classified from the output its program gave in an INFRA issue pod on
// 2026-11-18 (gcloud 594.0.0, bq 2.1.42, azure-cli 2.93.0, mise 2026.8.12, socat 1.8.0.3): a page
// the site answered, or for socat a handshake it completed, is an acceptance, a certificate refusal
// a refusal, and an error before any answer neither. bq writes its error to stdout, so its
// stand-in does too.
func TestImageOptionalTLSClientsClassifyTheirMeasuredOutput(t *testing.T) {
	clients := imageOptionalTLSClients(t, t.TempDir())
	for _, testCase := range []struct {
		client, name, stdout, stderr, exit string
		want                               tlsStatus
	}{
		{client: "gcloud", name: "control page", exit: "1", want: tlsConnectionAccepted,
			stderr: `ERROR: (gcloud.compute.regions.list) Some requests did not succeed: - <!doctype html><html lang=en>...Example Domain...`},
		{client: "gcloud", name: "emSign page through an SDK that accepts the chain", exit: "1", want: tlsConnectionAccepted,
			stderr: `Error decoding response "Your client successfully connected to the site. That's bad!"`},
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

	optional := imageOptionalTLSClients(t, t.TempDir())
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
// directory, which holds the token it signs in with; every optional client but socat, which keeps
// no state, keeps its home there, so nothing lands in the runtime user's. mise names the site
// inside its one tool argument, and keeps each site's download apart. socat names the site's host
// and https port in its OPENSSL: address.
func TestImageOptionalTLSClientsTargetTheSiteFromTheStateDirectory(t *testing.T) {
	stateDir, remove, err := newTLSStateDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
	clients := imageOptionalTLSClients(t, stateDir)
	inStateDir := func(path string) bool { return strings.HasPrefix(path, stateDir+string(os.PathSeparator)) }
	miseData := map[string]bool{}

	for _, site := range tlsSites {
		for name, client := range clients {
			if _, _, env := client.command(site); name != "socat" && !inStateDir(envValue(env, "HOME")) {
				t.Errorf("%s at %s has HOME %q, want a directory under %s", name, site.name, envValue(env, "HOME"), stateDir)
			}
		}

		_, _, env := clients["gcloud"].command(site)
		if got, want := envValue(env, "CLOUDSDK_API_ENDPOINT_OVERRIDES_COMPUTE"), site.url+"compute/v1/"; got != want {
			t.Errorf("gcloud at %s has compute endpoint %q, want %q", site.name, got, want)
		}
		if config := envValue(env, "CLOUDSDK_CONFIG"); !inStateDir(config) {
			t.Errorf("gcloud at %s has CLOUDSDK_CONFIG %q, want a directory under %s", site.name, config, stateDir)
		}
		if token, err := os.ReadFile(envValue(env, "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE")); err != nil || len(token) == 0 {
			t.Errorf("gcloud at %s reads its access token from %q: %q, %v; want a token the state directory holds", site.name, envValue(env, "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE"), token, err)
		}

		_, args, env := clients["mise"].command(site)
		if want := "http:legion-tls-probe[url=" + site.url + "]@1"; !slices.Contains(args, want) {
			t.Errorf("mise at %s runs %q, want the tool argument %q", site.name, args, want)
		}
		miseData[envValue(env, "MISE_DATA_DIR")] = true

		if _, args, _ := clients["socat"].command(site); !slices.Equal(args, []string{"-u", "-", "OPENSSL:" + tlsSiteHosts[site] + ":443"}) {
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
	var chromium tlsChromiumClient
	found := false
	for _, client := range imageTLSClients("omp", nil, "", "") {
		if client.name() == "chromium" {
			chromium, found = client.(tlsChromiumClient)
		}
	}
	if !found {
		t.Fatal("imageTLSClients() has no chromium client of type tlsChromiumClient")
	}
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
	clients := imageTLSClients("omp", nil, "", "")
	chromium, ok := clients[len(clients)-1].(tlsChromiumClient)
	if !ok {
		t.Fatalf("last image TLS client = %T, want tlsChromiumClient", clients[len(clients)-1])
	}
	chromium.command = func(tlsSite) (string, []string, []string) {
		return "sh", []string{"-c", "exec sleep 21"}, nil
	}

	outcome := chromium.probe(context.Background(), controlTLSSite)
	if outcome.status != tlsConnectionAccepted {
		t.Fatalf("Chromium probe after a 21s cold start = %+v, want accepted", outcome)
	}
}
