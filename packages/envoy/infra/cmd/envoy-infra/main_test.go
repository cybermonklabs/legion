package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/routes"
	"github.com/sjawhar/envoy/infra/seed"
	"github.com/sjawhar/envoy/internal/cmdtest"
)

func TestMain(m *testing.M) { os.Exit(cmdtest.Main(m)) }

// buildCLI builds this package's binary as name with the extra go build arguments args, and
// returns its path.
func buildCLI(t *testing.T, name string, args ...string) string {
	t.Helper()
	return cmdtest.BuildCommand(t, cmdtest.Command{Name: name, Flags: args})
}

// awsEnv is the whole environment of the binary run against endpoint: dummy static credentials, a
// home with no AWS files and no instance metadata, so the SDK reaches nothing but endpoint.
func awsEnv(t *testing.T, endpoint string) []string {
	t.Helper()
	home := t.TempDir()
	return []string{
		"PATH=/usr/bin:/bin", "HOME=" + home,
		"AWS_ENDPOINT_URL=" + endpoint, "AWS_ACCESS_KEY_ID=AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY=example-signing-key",
		"AWS_CONFIG_FILE=" + filepath.Join(home, "config"), "AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(home, "credentials"),
		"AWS_EC2_METADATA_DISABLED=true",
	}
}

// TestVersionPrintsTheStampedRelease: release-envoy-infra.yaml's build job stamps the CLI with
// `-ldflags "-X main.version=<X.Y.Z>"`, so a build stamped that way prints exactly that version
// and nothing else, without any AWS configuration. Any other build prints 0.0.0-dev, as the plugin
// does, rather than naming a release it is not.
func TestVersionPrintsTheStampedRelease(t *testing.T) {
	stamped := buildCLI(t, "stamped", "-ldflags", "-X main.version=9.9.9")
	local := buildCLI(t, "local")

	for _, c := range []struct{ binary, want string }{{stamped, "9.9.9\n"}, {local, "0.0.0-dev\n"}} {
		var stdout, stderr bytes.Buffer
		cmd := exec.Command(c.binary, "--version")
		cmd.Env = []string{"PATH=/usr/bin:/bin"} // no HOME, AWS_* or shared AWS files
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s --version: %v; stderr %q", c.binary, err, stderr.String())
		}
		if stdout.String() != c.want || stderr.Len() != 0 {
			t.Errorf("%s --version: stdout %q, stderr %q; want stdout %q and no stderr", c.binary, stdout.String(), stderr.String(), c.want)
		}
	}
}

// noHub fails the test if a command opens a hub: only `envoy-infra routes record` and `check` read
// one.
func noHub(t *testing.T) routes.Opener {
	return func(routes.Target) (routes.Source, error) {
		t.Helper()
		t.Fatal("the command opened a hub")
		return nil, nil
	}
}

// noStore fails the test if a command opens a store: neither --version nor the usage reads one.
func noStore(t *testing.T) seed.Stores {
	return seed.Stores{seed.AWSSecretsManager: {Open: func(context.Context, string, string) (seed.Store, error) {
		t.Error("a store was opened")
		return nil, errors.New("no store in this test")
	}}}
}

// brokenStdout fails every write, as stdout on a full disk (/dev/full) does. A closed stdout does
// not: the Go runtime opens it on the null device, where writes succeed.
type brokenStdout struct{}

func (brokenStdout) Write([]byte) (int, error) {
	return 0, errors.New("write /dev/stdout: bad file descriptor")
}

// TestVersionItCannotPrintExitsOne: a --version whose answer never reached stdout does not exit 0,
// so a script that reads it is not told the version was printed.
func TestVersionItCannotPrintExitsOne(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"--version"}, brokenStdout{}, &stderr, noStore(t), noHub(t)); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if want := "envoy-infra: could not print the version: write /dev/stdout: bad file descriptor\n"; stderr.String() != want {
		t.Errorf("stderr %q, want %q", stderr.String(), want)
	}
}

// TestVersionStandsAlone: --version followed by another word is a usage error, which echoes
// neither word, since a seed pasted there must not reach the terminal.
func TestVersionStandsAlone(t *testing.T) {
	const canary = "SUCANARYNOTASEED"
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--version", canary}, &stdout, &stderr, noStore(t), noHub(t)); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout %q, want none", stdout.String())
	}
	if !strings.Contains(stderr.String(), "envoy-infra: the command is seed, render or routes, or --version alone\n") {
		t.Errorf("stderr %q does not say what the command takes", stderr.String())
	}
	if strings.Contains(stderr.String(), canary) {
		t.Errorf("stderr echoes the word given: %q", stderr.String())
	}
}

// TestHelpNamesVersion: the usage names --version beside the seed, render and routes commands' own
// usage.
func TestHelpNamesVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, &stdout, &stderr, noStore(t), noHub(t)); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	for _, line := range []string{
		"usage: envoy-infra --version\n",
		"usage: envoy-infra seed {check|generate|verify}",
		"usage: envoy-infra render {nats-start-script|webhook-listener-env|metric-filters}",
		"usage: envoy-infra routes record --namespace <ns> --statefulset <name> --out <file>",
		"       envoy-infra routes check --namespace <ns> --statefulset <name> --record <file>",
	} {
		if !strings.Contains(stderr.String(), line) {
			t.Errorf("the usage lacks %q: %q", line, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "the command is seed") {
		t.Errorf("--help reports a usage error: %q", stderr.String())
	}
}

// TestTheCommandKnowsBothStores: the stores main hands the seed command are AWS Secrets Manager and
// GCP Secret Manager, as its usage error for another --store names them.
func TestTheCommandKnowsBothStores(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"seed", "check", "--store", "elsewhere", "--name", "example"}, &stdout, &stderr, stores, noHub(t))
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if want := "envoy-infra seed: --store is one of: aws-secrets-manager, gcp-secret-manager\n"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr %q lacks %q", stderr.String(), want)
	}
}

// TestRoutesOpensTheHubItIsGiven: `envoy-infra routes` runs the routes command over the opener main
// gives it, with the hub its flags name, and reports that opener's error; main's own opener is
// routes.OpenKubectl.
func TestRoutesOpensTheHubItIsGiven(t *testing.T) {
	var stdout, stderr bytes.Buffer
	var opened routes.Target
	open := func(target routes.Target) (routes.Source, error) {
		opened = target
		return nil, errors.New("no kubeconfig here")
	}
	code := run(context.Background(), []string{"routes", "record", "--namespace", "envoy", "--statefulset", "envoy-nats", "--out", filepath.Join(t.TempDir(), "r.json")},
		&stdout, &stderr, noStore(t), open)
	if code != 2 || opened.Namespace != "envoy" || opened.StatefulSet != "envoy-nats" || opened.Kubectl != "kubectl" {
		t.Fatalf("exit %d, opened %+v; want 2 and envoy/envoy-nats through kubectl", code, opened)
	}
	if want := "envoy-infra routes record: no kubeconfig here\n"; stderr.String() != want {
		t.Errorf("stderr %q, want %q", stderr.String(), want)
	}
}

// TestRenderPrintsToStdout: `envoy-infra render` runs the render command, which opens no store.
func TestRenderPrintsToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	args := []string{"render", "metric-filters", "--component", "nats-service", "--metric-namespace", "example/NATS",
		"--logical-name-prefix", "example-bus", "--json"}
	if code := run(context.Background(), args, &stdout, &stderr, noStore(t), noHub(t)); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"logicalName": "example-bus-route-disconnect-filter"`) || stderr.Len() != 0 {
		t.Errorf("stdout %q, stderr %q; want the NATS service's filters and no stderr", stdout.String(), stderr.String())
	}
}

// calloutUsers are the users of the nats-start-script rows below: a client with a seed, the session
// user whom a client presenting no credential becomes, and the auth service's user. Public keys
// only; their seeds were discarded when they were made.
const calloutUsers = `"users": [
    {"name": "example-hub", "nkey": "UCVLUXHFBITELHZUNIQG35K5PGQJIP6RX4LUG32L5I22JL7VGXKJQ4BV", "publish": {"allow": ["notifications.>"]}, "subscribe": {"allow": ["_INBOX.>"]}, "hasSeed": true},
    {"name": "example-sessions", "nkey": "UAZUULVPMGDGSEXFS26DBYEAMLBGWFX6HKOCQGIWTDBZARXTK3VRF5W5", "publish": {"allow": ["notifications.agent.>"]}, "subscribe": {"allow": ["_INBOX.>"]}},
    {"name": "example-auth", "nkey": "UD5BKU3BU7COAOUCXHGXWOSLNVLL4TEXCWPJHDUQE4C47ZNOYFOZVKUN", "publish": {"allow": ["$SYS._INBOX.>"]}, "subscribe": {"allow": ["$SYS.REQ.USER.AUTH"]}, "hasSeed": true}
  ]`

const calloutJSON = `"callout": {"issuer": "AB2TCXFBYSIN24KFPVJARBN5CHCL4ZCV5SBWSONTZ2WYXS3XTAEK6S3G", "xkey": "XBVCTZK3PV5Y7TBIVYTDZBP3O2YGHUUL7MMENN4DTZIREJCPGLTAF6EL", "serviceUser": "example-auth"}`

// startScriptWithout is the script `envoy-infra render nats-start-script` printed, at
// cybermonklabs/legion 545b211633 (before the callout existed), for calloutUsers with the session
// user as noAuthUser, `--config-header x --server-arg -js --config-path /tmp/n.conf`.
const startScriptWithout = `set -eu; printf '%s' '# x
authorization {
  users: [
    # example-hub
    {"nkey": "UCVLUXHFBITELHZUNIQG35K5PGQJIP6RX4LUG32L5I22JL7VGXKJQ4BV", "permissions": {"publish": {"allow": ["notifications.>"]}, "subscribe": {"allow": ["_INBOX.>"]}}}
    # example-sessions
    {"nkey": "UAZUULVPMGDGSEXFS26DBYEAMLBGWFX6HKOCQGIWTDBZARXTK3VRF5W5", "permissions": {"publish": {"allow": ["notifications.agent.>"]}, "subscribe": {"allow": ["_INBOX.>"]}}}
    # example-auth
    {"nkey": "UD5BKU3BU7COAOUCXHGXWOSLNVLL4TEXCWPJHDUQE4C47ZNOYFOZVKUN", "permissions": {"publish": {"allow": ["$SYS._INBOX.>"]}, "subscribe": {"allow": ["$SYS.REQ.USER.AUTH"]}}}
  ]
}
# example-sessions: a client presenting no credential becomes this user
no_auth_user: "UAZUULVPMGDGSEXFS26DBYEAMLBGWFX6HKOCQGIWTDBZARXTK3VRF5W5"
' > /tmp/n.conf; exec nats-server --config /tmp/n.conf -js`

// TestRenderNatsStartScriptWithACallout: the authorization file takes the callout and
// privateProjects as core.Authorization spells them. With a callout the script's configuration
// gains the auth_callout block, listing every keyed user's key, and no authorization timeout;
// without one it is byte for byte the script the command printed before the callout existed. A
// callout without an xkey is refused, and so are privateProjects beside a session user and beside
// a service user granted more than an auth service needs, each named.
func TestRenderNatsStartScriptWithACallout(t *testing.T) {
	render := func(t *testing.T, authorization string) (string, string, int) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "a.json")
		if err := os.WriteFile(path, []byte(authorization), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := []string{"render", "nats-start-script", "--authorization-file", path,
			"--config-header", "x", "--server-arg", "-js", "--config-path", "/tmp/n.conf"}
		code := run(context.Background(), args, &stdout, &stderr, noStore(t), noHub(t))
		return stdout.String(), stderr.String(), code
	}

	t.Run("with a callout", func(t *testing.T) {
		stdout, stderr, code := render(t, `{`+calloutUsers+`, "noAuthUser": "example-sessions", `+calloutJSON+`}`)
		if code != 0 || stderr != "" {
			t.Fatalf("exit %d, stderr %q; want 0 and no stderr", code, stderr)
		}
		want := strings.Replace(startScriptWithout, "  ]\n}\n", "  ]\n"+
			"  auth_callout {\n"+
			`    issuer: "AB2TCXFBYSIN24KFPVJARBN5CHCL4ZCV5SBWSONTZ2WYXS3XTAEK6S3G"`+"\n"+
			`    auth_users: ["UCVLUXHFBITELHZUNIQG35K5PGQJIP6RX4LUG32L5I22JL7VGXKJQ4BV", "UAZUULVPMGDGSEXFS26DBYEAMLBGWFX6HKOCQGIWTDBZARXTK3VRF5W5", "UD5BKU3BU7COAOUCXHGXWOSLNVLL4TEXCWPJHDUQE4C47ZNOYFOZVKUN"]`+"\n"+
			`    xkey: "XBVCTZK3PV5Y7TBIVYTDZBP3O2YGHUUL7MMENN4DTZIREJCPGLTAF6EL"`+"\n"+
			"  }\n"+
			"}\n", 1)
		// The ping lines end the configuration; the comment's apostrophe is quoted for the shell's
		// single-quoted printf argument.
		want = strings.Replace(want, "\n' > /tmp/n.conf", "\n"+
			`# a replica of the auth service that answers nothing is dropped from the callout'"'"'s queue group within 80 s`+"\n"+
			`ping_interval: "20s"`+"\n"+
			"ping_max: 2\n"+
			"' > /tmp/n.conf", 1)
		if stdout != want {
			t.Errorf("stdout:\n%s\nwant:\n%s", stdout, want)
		}
	})
	t.Run("without a callout, the script printed before callouts existed", func(t *testing.T) {
		stdout, stderr, code := render(t, `{`+calloutUsers+`, "noAuthUser": "example-sessions"}`)
		if code != 0 || stderr != "" {
			t.Fatalf("exit %d, stderr %q; want 0 and no stderr", code, stderr)
		}
		if stdout != startScriptWithout {
			t.Errorf("stdout:\n%s\nwant:\n%s", stdout, startScriptWithout)
		}
	})
	refused := func(t *testing.T, authorization, want string) {
		t.Helper()
		stdout, stderr, code := render(t, authorization)
		want = "refused: --authorization-file: authorization: " + want + "\n"
		if code != 2 || stdout != "" || stderr != want {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 2, no stdout and stderr %q", code, stdout, stderr, want)
		}
	}
	t.Run("a callout without an xkey is refused", func(t *testing.T) {
		refused(t, `{`+calloutUsers+`, "callout": {"issuer": "AB2TCXFBYSIN24KFPVJARBN5CHCL4ZCV5SBWSONTZ2WYXS3XTAEK6S3G", "serviceUser": "example-auth"}}`,
			"callout.xkey is required: without it nats-server sends each connecting client's token to the auth service unsealed, on $SYS.REQ.USER.AUTH")
	})
	t.Run("private projects beside a session user are refused, naming it", func(t *testing.T) {
		refused(t, `{`+calloutUsers+`, "noAuthUser": "example-sessions", `+calloutJSON+`, "privateProjects": true}`,
			`privateProjects: users[1] "example-sessions" is noAuthUser: a client presenting no credential would become it while a project is private`)
	})
	// The service user connects by its seed alone, so with a grant wider than an auth service's it
	// is a client past the service (review of #4118 at e7c174fa1b, its e-broad-service.json with an
	// xkey).
	t.Run("private projects beside a service user granted everything are refused, naming the entry", func(t *testing.T) {
		refused(t, `{"users": [{"name": "example-auth", "nkey": "UD5BKU3BU7COAOUCXHGXWOSLNVLL4TEXCWPJHDUQE4C47ZNOYFOZVKUN", "publish": {"allow": [">"]}, "subscribe": {"allow": [">"]}, "hasSeed": true}], `+
			calloutJSON+`, "privateProjects": true}`,
			`privateProjects: callout.serviceUser "example-auth": publish.allow[0] ">" is wider than the auth service's grant, which subscribes $SYS.REQ.USER.AUTH and publishes only its replies under $SYS._INBOX.; with a wider grant the service user is a client that connects past the auth service`)
	})
}

// TestAnInterruptDuringTheWriteEndsAsAnErrorAfterWriting: an interrupt while PutSecretValue is in
// flight cancels the call rather than killing the process, so generate ends as any write whose
// answer never came does: exit 1, the public key it printed before the write, `version id:
// unknown`, and the error telling the operator to check the secret.
func TestAnInterruptDuringTheWriteEndsAsAnErrorAfterWriting(t *testing.T) {
	const name = "example/envoy/nats-nkey-seed-example-host-a"
	binary := buildCLI(t, "envoy-infra")
	putArrived, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		switch r.Header.Get("X-Amz-Target") {
		case "secretsmanager.DescribeSecret":
			_, _ = w.Write([]byte(`{"ARN": "arn:aws:secretsmanager:us-east-1:000000000000:secret:` + name + `", "Name": "` + name + `"}`))
		case "secretsmanager.ListSecretVersionIds":
			_, _ = w.Write([]byte(`{"Versions": []}`))
		case "secretsmanager.PutSecretValue":
			close(putArrived)
			select { // the write hangs until the client gives up on it, or the test ends
			case <-r.Context().Done():
			case <-release:
			}
		default:
			t.Errorf("unexpected request %q", r.Header.Get("X-Amz-Target"))
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	defer close(release)

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(binary, "seed", "generate", "--store", "aws-secrets-manager", "--name", name, "--region", "us-east-1")
	cmd.Env = awsEnv(t, server.URL)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-putArrived:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("PutSecretValue never arrived; stderr %q", stderr.String())
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("generate ended with %v, want exit 1; stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}
	key, rest, _ := strings.Cut(strings.TrimPrefix(stdout.String(), "public key: "), "\n")
	if !core.IsUserPublicKey(key) || rest != "version id: unknown\n" {
		t.Errorf("stdout %q, want the public key and `version id: unknown`", stdout.String())
	}
	if want := "error after writing: put-secret-value " + name + " failed: canceled; whether a version was written is unknown, run check\n"; stderr.String() != want {
		t.Errorf("stderr %q, want %q", stderr.String(), want)
	}
}

// TestGenerateRefusesANullStdout: generate whose stdout is the null device would print the public
// key nowhere and write the seed, so it refuses before any call. A closed stdout is the same case,
// since the Go runtime opens a closed fd 0-2 on the null device at start (runtime/fds_unix.go).
func TestGenerateRefusesANullStdout(t *testing.T) {
	const name = "example/envoy/nats-nkey-seed-example-host-a"
	binary := buildCLI(t, "envoy-infra")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("generate made a call with a null stdout: %q", r.Header.Get("X-Amz-Target"))
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	env := awsEnv(t, server.URL)
	args := []string{"seed", "generate", "--store", "aws-secrets-manager", "--name", name, "--region", "us-east-1"}
	want := "refused: " + name + ": stdout is the null device, where the public key would be lost (a closed stdout is " +
		"one too: Go opens it on /dev/null); send stdout somewhere you can read. Nothing was written\n"
	for _, c := range []struct {
		name string
		cmd  *exec.Cmd
	}{
		// os/exec gives a command with no Stdout the null device.
		{"stdout on /dev/null", exec.Command(binary, args...)},
		{"stdout closed", exec.Command("/bin/sh", append([]string{"-c", `exec "$0" "$@" >&-`, binary}, args...)...)},
	} {
		t.Run(c.name, func(t *testing.T) {
			var stderr bytes.Buffer
			c.cmd.Env, c.cmd.Stderr = env, &stderr
			err := c.cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 {
				t.Fatalf("generate ended with %v, want exit 2; stderr %q", err, stderr.String())
			}
			if stderr.String() != want {
				t.Errorf("stderr %q, want %q", stderr.String(), want)
			}
		})
	}
}
