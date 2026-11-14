package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

const launcherUsage = "usage: legion launcher --connect tcp://host:port --sandbox <name> [--pod-uid <uid>] [--stop-grace <duration>] [--test-postgres] [--session-instance <project:region:instance> --session-database <name> --session-user <login>] (--roles-dir <dir> --role <role> [--role <role>…] | --token-file <path> --role <role> --private-dir <dir>)"

// roleList collects a repeated --role: an issue pod's container names every resident role, the
// controller's pod exactly one.
type roleList []string

func (l *roleList) String() string { return strings.Join(*l, ",") }

func (l *roleList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// runLauncher is PID 1 of an issue pod's one container, which serves every resident role, or of
// the controller pod's one container, which serves its one role. It has no workflow policy: it
// only authenticates to the daemon, once per role, and starts or stops that role's worker-shim
// on daemon commands. It has two hidden subcommands of its own, which nothing but this binary
// runs: `role-init`, the init of a role's namespaces, and `volume-trim-delete`, the deleter one
// sweep of the pod's cache trimmer makes its removals through.
func runLauncher(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case launcher.RoleInitSubcommand:
			return runRoleInit(ctx, args[1:], stdout, stderr)
		case launcher.VolumeTrimDeleteSubcommand:
			return runVolumeTrimDelete(args[1:], stdout, stderr)
		}
	}
	flags := newFlags("launcher", launcherUsage, stderr)
	connect := flags.String("connect", "", "the daemon launcher stream: tcp://host:port")
	tokenFile := flags.String("token-file", "", "the role-private launcher token file (the controller pod's one role)")
	sandbox := flags.String("sandbox", "", "the issue Sandbox name")
	var roles roleList
	flags.Var(&roles, "role", "a role this launcher serves; repeated with --roles-dir for an issue pod's roles")
	rolesDir := flags.String("roles-dir", "", "an issue pod's container-level per-role root: each role's launcher token, gh bundle and private, state and agent-secrets directories under <dir>/<role>")
	podUID := flags.String("pod-uid", os.Getenv("POD_UID"), "the current Kubernetes pod UID (POD_UID by default)")
	privateDir := flags.String("private-dir", "", "the controller container's own memory-backed directory for each generation's credentials")
	stopGrace := flags.Duration("stop-grace", 0, "container shutdown grace (30s when unset)")
	sessionInstance := flags.String("session-instance", "", "the Cloud SQL instance holding the session database: project:region:instance")
	sessionDatabase := flags.String("session-database", "", "the session database in that instance")
	sessionUser := flags.String("session-user", "", "the IAM login this pod signs in to the session database as")
	testPostgres := flags.Bool("test-postgres", false, "serve the pod's own PostgreSQL for tests from the worker image's legion-testpg, before any role starts and again whenever its postmaster exits (an issue pod's container, with --roles-dir)")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if len(flags.Args()) != 0 {
		fmt.Fprintln(stderr, launcherUsage)
		return 2
	}
	cfg := launcher.Config{
		Connect: *connect, Sandbox: *sandbox, PodUID: *podUID, StopGrace: *stopGrace,
		Stdout: stdout, Stderr: stderr,
		SessionDatabase: ompsessions.Database{Instance: *sessionInstance, Name: *sessionDatabase, User: *sessionUser},
	}
	if *rolesDir != "" {
		cfg.RolesDir, cfg.Roles, cfg.TestPostgres = *rolesDir, roles, *testPostgres
		if *tokenFile != "" || *privateDir != "" {
			fmt.Fprintln(stderr, "launcher: --roles-dir holds every role's own launcher token and private directory: no --token-file or --private-dir")
			return 2
		}
	} else if *testPostgres {
		// The controller's pod runs no tests, and its container is not where the server belongs.
		fmt.Fprintln(stderr, "launcher: --test-postgres is an issue pod's container: it needs --roles-dir")
		return 2
	} else {
		if len(roles) > 1 {
			fmt.Fprintln(stderr, "launcher: several roles need --roles-dir, which holds each one's own token and private directory")
			return 2
		}
		if len(roles) == 1 {
			cfg.Role = roles[0]
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
}

const roleInitUsage = "usage: legion launcher role-init --roles-dir <dir> --role <role> --generation <n> [--home <dir>] [--home-mounts <dir>] [--fixed-root <dir>] --status-fd <n> -- <shim argv…>"

// runRoleInit is `legion launcher role-init`, the launcher's own hidden subcommand: the init of
// one role generation's user, mount, PID and IPC namespaces, which the launcher clones into them
// with the generation's environment. It is never run by hand, and `legion launcher --help` does
// not offer it: run outside those namespaces it refuses, since it would otherwise mount a role's
// private view over the container's own. ctx ends at the launcher's stop (main's SIGTERM), which
// it forwards to the role. A failure before the shim started exits
// launcher.RoleInitSetupFailedExit, the one status the launcher reports as a setup failure.
func runRoleInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("launcher role-init", roleInitUsage, stderr)
	// The cover this role's namespace is built over, registered from the one table the deleter's
	// own flags are registered from too (launcher.RoleCoverFlags), so neither command can be
	// given a directory the other is not.
	cover := launcher.RoleCoverFlags(flags)
	role := flags.String("role", "", "the role whose namespaces this process is the init of")
	generation := flags.Uint64("generation", 0, "the generation this role-init runs, which names its own overlay work directory")
	statusFD := flags.Int("status-fd", 0, "the descriptor to report the shim's own wait status on")
	// A role-init the launcher started with flags it cannot read failed before its shim as much as
	// one whose mounts failed, so it exits the same status; only --help, which nothing runs, exits 0.
	if code, ok := parseFlags(flags, args); !ok {
		if code == 0 {
			return 0
		}
		return launcher.RoleInitSetupFailedExit
	}
	if len(flags.Args()) == 0 {
		fmt.Fprintln(stderr, roleInitUsage)
		return launcher.RoleInitSetupFailedExit
	}
	if err := launcher.RoleInit(ctx, launcher.RoleInitConfig{
		RoleCover: cover(), Role: *role, Generation: *generation, StatusFD: *statusFD,
		Argv: flags.Args(), Stdout: stdout, Stderr: stderr,
	}); err != nil {
		fmt.Fprintln(stderr, err)
		return launcher.RoleInitSetupFailedExit
	}
	return 0
}

const volumeTrimDeleteUsage = "usage: legion launcher volume-trim-delete --roles-dir <dir> [--home <dir>] [--home-mounts <dir>] [--fixed-root <dir>] (the cache trimmer's own deleter: the daemon's launcher runs it, never a person)"

// runVolumeTrimDelete is `legion launcher volume-trim-delete`, the launcher's second hidden
// subcommand: the process one sweep of the pod's cache trimmer makes every one of its removals
// through, in a user, mount, PID and IPC namespace of its own, built over the same cover a role's
// namespace is built over: every path a role is given something of its own at is an empty
// filesystem of the deleter's, and /legion, where the deletions are made, is left as the
// container mounted it. The trimmer starts it and hands it the descriptor of every directory the
// sweep listed; it reads one request per line on its standard input and answers one line on its
// standard output, and `legion launcher --help` does not offer it.
//
// Nothing a person runs reaches it: run by hand it holds no descriptor of the volume and is not
// the init of a PID namespace of its own, and it refuses rather than sitting there reading
// requests with nothing covered (launcher.VolumeTrimDelete). ctx is not taken because the sweep
// ends it by closing the requests, and kills it if it outlives the grace.
func runVolumeTrimDelete(args []string, stdout, stderr io.Writer) int {
	flags := newFlags("launcher volume-trim-delete", volumeTrimDeleteUsage, stderr)
	// The same registration role-init makes, so the deleter is told the cover whole.
	cover := launcher.RoleCoverFlags(flags)
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if len(flags.Args()) != 0 {
		fmt.Fprintln(stderr, volumeTrimDeleteUsage)
		return 1
	}
	if err := launcher.VolumeTrimDelete(cover(), os.Stdin, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
