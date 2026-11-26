package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/cipher"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubproxy"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

const forwarderUsage = "legion forwarder --role <role> --listen <127.0.0.1:port> --secret <NAME>... --reason <text> --github-host <host>... --gh-dir <dir> [--default-owner <owner>] [--rename <from>=<to>]...  (a role's GitHub forwarder; its shim runs it, never a person)"

// newForwarderCommand is `legion forwarder`, a role's GitHub forwarder (internal/githubproxy): a
// loopback HTTP CONNECT proxy the role's environment names in HTTPS_PROXY and https_proxy, which
// sends a CONNECT to a host the role's grant binds to the secrets broker's proxy port with the
// grant's password, and opens every other CONNECT itself. It asks the broker for the grant with
// the role's own key, the one AGENT_SECRETS_KEY_DIR holds once the daemon has enrolled it, at
// the broker AGENT_SECRETS_URL names: --secret's names for an hour, under --reason, with no
// target. It writes each account's placeholder into the gh bundle at --gh-dir, one directory per
// account, which LEGION_GH_OWNERS_DIR names, beside --rename's pairs, with --default-owner's
// directory the one GH_CONFIG_DIR names. Before its first grant a CONNECT to a --github-host
// waits for it, and every other CONNECT goes out at once. It asks for the next grant with ten
// minutes of the hour left, and at once when the broker's proxy answers 407, and revokes the
// grants it holds when it stops. The role's shim starts it with the flags the daemon wrote for the
// generation, from the deployment's runtime.kubernetes.agent_secrets.github and github_hosts; its
// lines go to stderr.
func newForwarderCommand(stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    shimwire.ForwarderCommand,
		Short:  "a role's GitHub forwarder: the loopback proxy that sends GitHub's hosts to the secrets broker's proxy under the role's grant (a role's shim runs it, never a person)",
		Args:   noArguments,
		Hidden: true,
	}
	var given forwarderFlags
	flags := cmd.Flags()
	flags.StringVar(&given.role, "role", "", "the role the forwarder serves, which its log lines name")
	flags.StringVar(&given.listen, "listen", "", "the loopback address the role's HTTPS_PROXY names, 127.0.0.1:<port>")
	flags.StringArrayVar(&given.secrets, "secret", nil, "a secret name of the role's behind the broker's proxy (repeatable)")
	flags.StringVar(&given.reason, "reason", "", "the reason each grant is asked under, which names the role's issue")
	flags.StringArrayVar(&given.githubHosts, "github-host", nil, "a host the broker's GitHub kind binds, which waits for the first grant (repeatable)")
	flags.StringVar(&given.ghDir, "gh-dir", "", "the gh bundle root the placeholders are written into (LEGION_GH_OWNERS_DIR)")
	flags.StringVar(&given.defaultOwner, "default-owner", "", "the account whose directory GH_CONFIG_DIR names")
	flags.StringArrayVar(&given.renames, "rename", nil, "a configured rename <from-owner>/<repo>=<to-owner>/<repo> the bundle routes by (repeatable)")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		return runForwarder(ctx, given, os.Getenv, stderr)
	})
	return cmd
}

// forwarderFlags are the command's flag values, as parsed.
type forwarderFlags struct {
	role, listen, reason, ghDir, defaultOwner string
	secrets, githubHosts, renames             []string
}

// runForwarder checks the flags, binds the address and serves until ctx ends. A flag or variable
// the configuration refuses exits 2 with the usage, and a Config that githubproxy.Listen refuses
// (a listen address that is no loopback one) or a listener that fails exits 1; a broker that
// cannot be reached is no refusal, since the forwarder asks again until it holds a grant.
func runForwarder(ctx context.Context, given forwarderFlags, getenv func(string) string, stderr io.Writer) int {
	cfg, err := forwarderConfig(given, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "legion forwarder: %v\n%s\n", err, forwarderUsage)
		return 2
	}
	cfg.Log = slog.New(slog.NewTextHandler(stderr, nil))
	fwd, err := githubproxy.Listen(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "legion forwarder: %v\n", err)
		return 1
	}
	if err := fwd.Serve(ctx); err != nil {
		fmt.Fprintf(stderr, "legion forwarder: %v\n", err)
		return 1
	}
	return 0
}

// forwarderConfig is the forwarder's configuration from its flags and AGENT_SECRETS_URL and
// AGENT_SECRETS_KEY_DIR, the role's broker and key directory, which the runtime sets in every
// enrolled role's environment.
func forwarderConfig(given forwarderFlags, getenv func(string) string) (githubproxy.Config, error) {
	switch {
	case given.role == "":
		return githubproxy.Config{}, errors.New("--role is required")
	case given.listen == "":
		return githubproxy.Config{}, errors.New("--listen is required")
	case len(given.secrets) == 0:
		return githubproxy.Config{}, errors.New("--secret is required, once per name")
	case given.reason == "":
		return githubproxy.Config{}, errors.New("--reason is required")
	case len(given.githubHosts) == 0:
		return githubproxy.Config{}, errors.New("--github-host is required, once per host the broker's GitHub kind binds")
	case given.ghDir == "":
		return githubproxy.Config{}, errors.New("--gh-dir is required")
	}
	if given.defaultOwner != "" && !ghconfig.IsOwner(given.defaultOwner) {
		return githubproxy.Config{}, fmt.Errorf("--default-owner %q is no lowercased GitHub login", given.defaultOwner)
	}
	renames := make([]ghrepo.Rename, 0, len(given.renames))
	for _, pair := range given.renames {
		from, to, ok := strings.Cut(pair, "=")
		fromRepo, fromErr := ghrepo.Parse("--rename's from", from)
		toRepo, toErr := ghrepo.Parse("--rename's to", to)
		if !ok || fromErr != nil || toErr != nil {
			return githubproxy.Config{}, fmt.Errorf("--rename %q is no <owner>/<repo>=<owner>/<repo>", pair)
		}
		renames = append(renames, ghrepo.Rename{From: fromRepo, To: toRepo})
	}
	brokerURL, keyDir := getenv("AGENT_SECRETS_URL"), getenv("AGENT_SECRETS_KEY_DIR")
	if brokerURL == "" || keyDir == "" {
		return githubproxy.Config{}, errors.New("AGENT_SECRETS_URL and AGENT_SECRETS_KEY_DIR must name the role's broker and key directory")
	}
	return githubproxy.Config{
		Role: given.role, Listen: given.listen, Secrets: given.secrets, Reason: given.reason, GitHubHosts: given.githubHosts,
		GHDir: given.ghDir, DefaultOwner: given.defaultOwner, Renames: renames,
		Open: func() (githubproxy.Broker, error) { return cipher.LoadSession(brokerURL, keyDir) },
	}, nil
}
