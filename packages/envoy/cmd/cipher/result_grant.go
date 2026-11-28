package main

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/fold"
	"github.com/sjawhar/envoy/internal/redact"
)

// cmdResultGrant proves a GitHub workflow run at the broker's token endpoint and runs COMMAND
// with the proxy placeholder of the run's result grant (cipherclient.Client.AirlockPlaceholder),
// so the command's result writes reach Dispatch only through the broker's airlock proxy, which
// holds the run's results token and the key it is bound to. This process holds neither: the DPoP
// key it proves the trade with is made here, in memory, never written, and dies with the exec.
//
// Nothing runs when the trade is refused, when DISPATCH_URL names a host the grant does not bind or
// is not https on port 443 - the command's writes would go around the proxy with the placeholder in
// them - or when the airlock release cannot be prepared. In a GitHub job the placeholder, which is the proxy's
// password and which a proxy's error can quote, is masked in the job's log first.
func cmdResultGrant(args []string, stdout, stderr io.Writer) int {
	sep := -1
	for i, arg := range args {
		if arg == "--" {
			sep = i
			break
		}
	}
	before := args
	if sep >= 0 {
		before = args[:sep]
	}
	flags := newFlagSet("result-grant", stderr)
	if err := flags.Parse(before); err != nil {
		return exitUsage(err)
	}
	if flags.NArg() != 0 || sep < 0 || sep+1 >= len(args) {
		fmt.Fprintln(stderr, "agent-secrets result-grant: use result-grant -- COMMAND [ARGS...]")
		return exitUsageError
	}
	command := args[sep+1:]
	path, err := exec.LookPath(command[0])
	if err != nil {
		fmt.Fprintln(stderr, "agent-secrets result-grant: command not found")
		return exitNotFound
	}
	base := brokerBaseURL()
	if base.Empty() {
		fmt.Fprintln(stderr, "agent-secrets result-grant: AGENT_SECRETS_URL is required")
		return exitUsageError
	}
	source, err := cipherclient.MachineIdentity(os.Getenv)
	if err != nil || source == nil {
		fmt.Fprintln(stderr, "agent-secrets result-grant: a GitHub workflow identity is required")
		return exitUsageError
	}
	switch source.(type) {
	case *cipherclient.GitHubActionsIdentity, *cipherclient.WorkflowRunTokenFile:
	default:
		fmt.Fprintln(stderr, "agent-secrets result-grant: only a GitHub workflow identity can request results")
		return exitUsageError
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		fmt.Fprintln(stderr, "agent-secrets result-grant: could not make the key the trade is proved with")
		return 1
	}
	subject := &runIdentity{Source: source}
	client, err := cipherclient.New(cipherclient.Config{BrokerURL: base, Key: key, Subject: subject, NoLauncherCredential: true})
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets result-grant: %s\n", redact.OneLine(err.Error()))
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	grant, err := client.AirlockPlaceholder(ctx)
	if err != nil {
		if errors.Is(err, errNotAWorkflowRun) {
			fmt.Fprintln(stderr, "agent-secrets result-grant: only a GitHub workflow identity can request results")
			return exitUsageError
		}
		fmt.Fprintf(stderr, "agent-secrets result-grant: %s\n", redact.OneLine(subject.cut(err.Error())))
		return 1
	}
	release := airlockReleaseOf(grant.Release)
	hosts := release.Names[oauthwire.AirlockResultsName].Hosts
	dispatchURL := os.Getenv("DISPATCH_URL")
	host := dispatchHost(dispatchURL)
	if !proxyrules.HostBound(hosts, host) {
		fmt.Fprintf(stderr, "agent-secrets result-grant: DISPATCH_URL names %s, which the airlock does not bind (it binds %s), so the command's writes would go around it; nothing was run\n",
			redact.OneLine(cmp.Or(host, "no host")), strings.Join(hosts, ", "))
		return 1
	}
	if !throughTheProxy(dispatchURL) {
		// The child's HTTPS_PROXY covers https alone, and the proxy reads a bound host on 443
		// alone: an http URL would send the placeholder in the clear straight to the host, and
		// another port would tunnel the write past the proxy's checks.
		fmt.Fprintf(stderr, "agent-secrets result-grant: DISPATCH_URL names %s, which the airlock binds, but not as https on port 443, the one way the command's writes go through the airlock's proxy; otherwise they would go around it; nothing was run\n",
			redact.OneLine(host))
		return 1
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		// The runner reads this from the step's output and writes *** for the placeholder in
		// every later line of the job's log.
		fmt.Fprintf(stdout, "::add-mask::%s\n", grant.Placeholder)
	}
	// The command gets the placeholder and no identity of the run's own: buildChildEnv drops
	// every AGENT_SECRETS_* variable but sessionIdentityVars, AGENT_SECRETS_MACHINE_IDENTITY
	// among them, which would let an `agent-secrets` the command runs trade the run's token again.
	// It is released no value, so the trust decided and written is its inherited environment's,
	// as the exec form decides it (prepareProxyTrustForCommand).
	environ := os.Environ()
	files, err := prepareProxyTrustForCommand(environ, nil, release, grant.GrantID, &grant.ExpiresAt, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets result-grant: %s\n", redact.OneLine(err.Error()))
		return 1
	}
	environment, err := buildChildEnv(environ, nil, release, files)
	if err != nil {
		fmt.Fprintf(stderr, "agent-secrets result-grant: %s\n", redact.OneLine(err.Error()))
		return 1
	}
	if err := syscall.Exec(path, command, environment); err != nil {
		fmt.Fprintln(stderr, "agent-secrets result-grant: could not execute the command")
		return exitCannotRun
	}
	return 0
}

// errNotAWorkflowRun is runIdentity's refusal of a token the identity answered under another type
// than a GitHub workflow run's.
var errNotAWorkflowRun = errors.New("the identity is no GitHub workflow run")

// runIdentity is the run's identity as the trade reads it: a workflow run's token and nothing
// else, and each token it answered kept so that an error this command prints can be cut of it.
type runIdentity struct {
	cipherclient.Source
	given []string
}

func (r *runIdentity) Token(ctx context.Context) (string, string, error) {
	token, kind, err := r.Source.Token(ctx)
	if token != "" {
		r.given = append(r.given, token)
	}
	if err == nil && kind != oauthwire.SubjectTokenTypeGitHub {
		return "", "", errNotAWorkflowRun
	}
	return token, kind, err
}

// cut is text with every token the identity answered written as [redacted].
func (r *runIdentity) cut(text string) string {
	for _, token := range r.given {
		text = strings.ReplaceAll(text, token, "[redacted]")
	}
	return text
}

// airlockReleaseOf is the token endpoint's airlock release as the exec form reads one.
func airlockReleaseOf(release oauthwire.AirlockRelease) *AirlockRelease {
	names := make(map[string]AirlockName, len(release.Names))
	for name, held := range release.Names {
		names[name] = AirlockName(held)
	}
	return &AirlockRelease{ProxyURL: release.ProxyURL, CACertificate: release.CACertificate, ProxyPassword: release.ProxyPassword, Names: names}
}

// dispatchHost is the host DISPATCH_URL names, without its port and ASCII lower-cased as the proxy
// folds a host (fold.ASCII), and "" for a value that is no URL with a host. Whether the grant binds
// it is the proxy's own rule, proxyrules.HostBound.
func dispatchHost(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return fold.ASCII(parsed.Hostname())
}

// throughTheProxy reports whether DISPATCH_URL is an https URL on port 443, written or left to the
// scheme: the one form of a write to a bound host that the child's HTTPS_PROXY sends to the airlock's
// proxy and the proxy reads (it decrypts a bound host on 443 alone).
func throughTheProxy(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && parsed.Scheme == "https" && (parsed.Port() == "" || parsed.Port() == "443")
}
