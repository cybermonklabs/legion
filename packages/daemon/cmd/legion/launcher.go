package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

// newLauncherCommand is PID 1 of an issue pod's one container, which serves every resident role,
// or of the controller pod's one container, which serves its one role. It has no workflow policy:
// it only authenticates to the daemon, once per role, and starts or stops that role's
// worker-shim on daemon commands. It has two hidden subcommands of its own, which `legion
// launcher --help` does not offer and which this binary starts itself: `role-init`, the init of a
// role's namespaces, and `volume-trim-delete`, the deleter one sweep of the pod's cache trimmer
// makes its removals through. Run by hand, each refuses unless it is the init of a PID namespace
// of its own (newRoleInitCommand, newVolumeTrimDeleteCommand). It takes no word of its own: a
// word is a command under it or a usage error naming the word, a flag after it included
// (wordFirst).
func newLauncherCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "launcher --connect tcp://host:port --sandbox <name> [--pod-uid <uid>] [--stop-grace <duration>] [--test-postgres] [--microvm [--metadata-workload-pool <project>.svc.id.goog --metadata-identity-provider <url>]] [--session-instance <project:region:instance> --session-database <name> --session-user <login> [--session-impersonate-service-account <email> [--session-writer-audience <provider>]]] (--roles-dir <dir> --role <role> [--role <role>…] | --token-file <path> --role <role> --private-dir <dir>)",
		Short:  "PID 1 of an issue pod's roles container or the controller's pod: starts and stops each role's worker-shim on the daemon's command",
		Hidden: true,
	}
	wordFirst(cmd)
	flags := cmd.Flags()
	connect := flags.String("connect", "", "the daemon launcher stream: tcp://host:port")
	tokenFile := flags.String("token-file", "", "the role-private launcher token file (the controller pod's one role)")
	sandbox := flags.String("sandbox", "", "the issue Sandbox name")
	// A repeated --role: an issue pod's container names every resident role, the controller's
	// pod exactly one.
	roles := flags.StringArray("role", nil, "a role this launcher serves; repeated with --roles-dir for an issue pod's roles")
	rolesDir := flags.String("roles-dir", "", "an issue pod's container-level per-role root: each role's launcher token, gh bundle and private, state and agent-secrets directories under <dir>/<role>")
	podUID := flags.String("pod-uid", os.Getenv("POD_UID"), "the current Kubernetes pod UID (POD_UID by default)")
	privateDir := flags.String("private-dir", "", "the controller container's own memory-backed directory for each generation's credentials")
	stopGrace := flags.Duration("stop-grace", 0, "container shutdown grace (30s when unset)")
	sessionInstance := flags.String("session-instance", "", "the Cloud SQL instance holding the session database: project:region:instance")
	sessionDatabase := flags.String("session-database", "", "the session database in that instance")
	sessionUser := flags.String("session-user", "", "the IAM login this pod signs in to the session database as")
	sessionAccount := flags.String("session-impersonate-service-account", "", "the Google service account the session proxy impersonates with this pod's own credentials and signs in as; optional, and given only with the three other session flags")
	sessionWriterAudience := flags.String("session-writer-audience", "", "the audience of the session writer token that only this launcher reads, a workload identity provider's full resource name: the session proxy exchanges that token to impersonate the service account, in place of this pod's own credentials, and admits only this launcher and the generation's agent; optional, and given only with the four other session flags and --roles-dir")
	testPostgres := flags.Bool("test-postgres", false, "serve the pod's own PostgreSQL for tests from the worker image's legion-testpg, before any role starts and again whenever its postmaster exits (an issue pod's container, with --roles-dir)")
	microVM := flags.Bool("microvm", false, "the pod runs in a GKE Sandbox microVM rather than under gVisor")
	metadataPool := flags.String("metadata-workload-pool", "", "serve the pod-local metadata endpoint for this Workload Identity pool, <project>.svc.id.goog, given with --metadata-identity-provider (a microVM issue pod's container: with --microvm and --roles-dir)")
	metadataProvider := flags.String("metadata-identity-provider", "", "the cluster's identity provider the metadata endpoint exchanges the pod's ServiceAccount token with: https://container.googleapis.com/v1/projects/<project>/locations/<location>/clusters/<cluster>")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int {
		cfg := launcher.Config{
			Connect: *connect, Sandbox: *sandbox, PodUID: *podUID, StopGrace: *stopGrace,
			Stdout: stdout, Stderr: stderr, MicroVM: *microVM,
			SessionDatabase: ompsessions.Database{
				Instance: *sessionInstance, Name: *sessionDatabase, User: *sessionUser, ImpersonateServiceAccount: *sessionAccount,
				WriterAudience: *sessionWriterAudience,
			},
		}
		// Either metadata flag names the endpoint, so one given without the other is refused as
		// named in part (launcher.Config's validate), as are both outside a microVM issue pod.
		if *metadataPool != "" || *metadataProvider != "" {
			cfg.Metadata = &launcher.MetadataConfig{WorkloadPool: *metadataPool, IdentityProvider: *metadataProvider}
		}
		if *rolesDir != "" {
			cfg.RolesDir, cfg.Roles, cfg.TestPostgres = *rolesDir, *roles, *testPostgres
			if *tokenFile != "" || *privateDir != "" {
				fmt.Fprintln(stderr, "launcher: --roles-dir holds every role's own launcher token and private directory: no --token-file or --private-dir")
				return 2
			}
		} else if *testPostgres {
			// The controller's pod runs no tests, and its container is not where the server belongs.
			fmt.Fprintln(stderr, "launcher: --test-postgres is an issue pod's container: it needs --roles-dir")
			return 2
		} else {
			if len(*roles) > 1 {
				fmt.Fprintln(stderr, "launcher: several roles need --roles-dir, which holds each one's own token and private directory")
				return 2
			}
			if len(*roles) == 1 {
				cfg.Role = (*roles)[0]
			}
			token, err := os.ReadFile(*tokenFile)
			if err != nil {
				fmt.Fprintf(stderr, "launcher: token file %s: %v\n", *tokenFile, err)
				return 1
			}
			cfg.Token, cfg.PrivateDir = strings.TrimSpace(string(token)), *privateDir
		}
		if err := launcher.Run(ctx, cfg); err != nil && ctx.Err() == nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	})
	cmd.AddCommand(newRoleInitCommand(stdout, stderr), newVolumeTrimDeleteCommand(stdout, stderr))
	return cmd
}

const roleInitUsage = "usage: legion launcher role-init --roles-dir <dir> --role <role> --generation <n> [--home <dir>] [--home-mounts <dir>] [--fixed-root <dir>] --status-fd <n> -- <shim argv…>"

// newRoleInitCommand is `legion launcher role-init`, the launcher's own hidden subcommand: the
// init of one role generation's user, mount, PID and IPC namespaces, which the launcher clones
// into them with the generation's environment. It is never run by hand, and `legion launcher
// --help` does not offer it: run outside those namespaces it refuses, since it would otherwise
// mount a role's private view over the container's own. ctx ends at the launcher's stop (main's
// SIGTERM), which it forwards to the role. A failure before the shim started exits
// launcher.RoleInitSetupFailedExit, the one status the launcher reports as a setup failure.
func newRoleInitCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    strings.TrimPrefix(roleInitUsage, "usage: legion launcher "),
		Short:  "the init of one role generation's namespaces (the launcher runs it, never a person)",
		Hidden: true,
	}
	flags := cmd.Flags()
	// The cover this role's namespace is built over, registered from the one table the deleter's
	// own flags are registered from too (launcher.RoleCoverFlags), so neither command can be
	// given a directory the other is not.
	cover := launcher.RoleCoverFlags(flags)
	role := flags.String("role", "", "the role whose namespaces this process is the init of")
	generation := flags.Uint64("generation", 0, "the generation this role-init runs, which names its own overlay work directory")
	statusFD := flags.Int("status-fd", 0, "the descriptor to report the shim's own wait status on")
	// A role-init the launcher started with flags it cannot read failed before its shim as much as
	// one whose mounts failed, so it exits the same status; only --help, which nothing runs, exits 0.
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		fmt.Fprintf(stderr, "legion launcher role-init: %v\n%s\n", err, roleInitUsage)
		return exitCode(launcher.RoleInitSetupFailedExit)
	})
	cmd.RunE = runs(func(ctx context.Context, args []string) int {
		argv, err := afterDash(cmd, args)
		if err != nil || len(argv) == 0 {
			if err != nil {
				fmt.Fprintf(stderr, "legion launcher role-init: %v\n", err)
			}
			fmt.Fprintln(stderr, roleInitUsage)
			return launcher.RoleInitSetupFailedExit
		}
		if err := launcher.RoleInit(ctx, launcher.RoleInitConfig{
			RoleCover: cover(), Role: *role, Generation: *generation, StatusFD: *statusFD,
			Argv: argv, Stdout: stdout, Stderr: stderr,
		}); err != nil {
			fmt.Fprintln(stderr, err)
			return launcher.RoleInitSetupFailedExit
		}
		return 0
	})
	return cmd
}

const volumeTrimDeleteUsage = "usage: legion launcher volume-trim-delete --roles-dir <dir> [--home <dir>] [--home-mounts <dir>] [--fixed-root <dir>] (the cache trimmer's own deleter, which the launcher starts: run by hand, it refuses unless it is the init of a PID namespace of its own)"

// newVolumeTrimDeleteCommand is `legion launcher volume-trim-delete`, the launcher's second hidden
// subcommand: the process one sweep of the pod's cache trimmer makes every one of its removals
// through, in a user, mount, PID and IPC namespace of its own, built over the same cover a role's
// namespace is built over: every directory a role is given something of its own at is an empty
// filesystem of the deleter's but /proc, a procfs of the deleter's own PID namespace, and
// /legion, where the deletions are made, is left as the container mounted it. The trimmer starts
// it and hands it the descriptor of every directory the sweep listed; it reads one request per
// line on its standard input and answers one line on its standard output, and
// `legion launcher --help` does not offer it.
//
// Run by hand in the container it refuses: it is not the init of a PID namespace of its own,
// and it stops before it reads a request rather than sitting there reading requests with nothing
// covered (launcher.VolumeTrimDelete). That check is the whole of the refusal, so a person who
// makes it PID 1 — under `unshare --user --map-root-user --pid --fork --mount`, say — gets a
// deleter that runs. It gains nothing by that: it holds only the descriptors that person handed
// it, and its namespaces give it no privilege outside them, so what it can remove is what the
// uid that started it could remove anyway. It reads no context: the sweep ends it by closing the
// requests, and kills it if it outlives the grace. It takes no word, so a word is a usage error.
func newVolumeTrimDeleteCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    strings.TrimPrefix(volumeTrimDeleteUsage, "usage: legion launcher "),
		Short:  "the cache trimmer's deleter, which one sweep makes its removals through (the launcher runs it, never a person)",
		Hidden: true,
		Args:   noArguments,
	}
	// The same registration role-init makes, so the deleter is told the cover whole.
	cover := launcher.RoleCoverFlags(cmd.Flags())
	cmd.RunE = runs(func(context.Context, []string) int {
		if err := launcher.VolumeTrimDelete(cover(), os.Stdin, stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	})
	return cmd
}
