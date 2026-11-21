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
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/redact"
)

// cmdResultGrant proves a GitHub workflow run at the broker's token endpoint and runs COMMAND
// with the airlock placeholder of the run's result grant (cipherclient.Client.AirlockPlaceholder),
// so the command's result writes reach Dispatch only through the broker's airlock proxy, which
// holds the run's results token and the key it is bound to. This process holds neither: the DPoP
// key it proves the trade with is made here, in memory, never written, and dies with the exec.
//
// Nothing runs when the trade is refused, when DISPATCH_URL names a host the grant does not bind -
// the command's writes would go around the proxy with the placeholder in them - or when the
// airlock release cannot be prepared. In a GitHub job the placeholder, which is the proxy's
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
	if host := dispatchHost(os.Getenv("DISPATCH_URL")); !bindsHost(hosts, host) {
		fmt.Fprintf(stderr, "agent-secrets result-grant: DISPATCH_URL names %s, which the airlock does not bind (it binds %s), so the command's writes would go around it; nothing was run\n",
			redact.OneLine(cmp.Or(host, "no host")), strings.Join(hosts, ", "))
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
	environ := os.Environ()
	files, err := prepareAirlock(environ, release, grant.GrantID, &grant.ExpiresAt)
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

// dispatchHost is the host DISPATCH_URL names, lower-cased and without its port, and "" for a
// value that is no URL with a host.
func dispatchHost(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// bindsHost reports whether host is one of the grant's hosts, as the proxy compares a host: a
// name exactly, or under a "*." entry one label or more below its domain.
func bindsHost(hosts []string, host string) bool {
	if host == "" {
		return false
	}
	for _, bound := range hosts {
		bound = strings.ToLower(bound)
		if bound == host || (strings.HasPrefix(bound, "*.") && strings.HasSuffix(host, bound[1:])) {
			return true
		}
	}
	return false
}
