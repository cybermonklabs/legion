package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/redact"
)

// newRoot is `legion`'s command line: every command with its flags, the ones only a program runs
// hidden from help, each writing to stdout and stderr. cobra prints help on stdout and nothing
// else: every error is run's to print, so a command that fails prints one line in one shape.
//
// What a command writes to stderr itself passes the URL rule (redact.Writer), so every URL in it
// keeps no userinfo, and no query but a `file:` URL's: no command can print a credential a URL
// carries, such as a remote's URL `git remote -v` lists, a flag's value or an error quoting
// either. Standard output stays as written, since it carries what a caller parses (`legion gh`'s
// answer, `legion git-credential`'s token to git). Three commands get stderr as it is as well,
// because they hand it to a child process as that child's own stream, which the rule must not
// touch: the launcher hands its streams to each role's shim (launcher.RoleInitConfig.Stderr),
// `legion claude-bridge` hands its stderr to Claude Code, and `legion controller start` hands it
// to its Oh My Pi probe's foreground attempts, where a launch prefix writes its prompt. Each of the
// three passes its own lines through the rule itself (the launcher's roleLog and its subcommands'
// writers, the bridge's log and printRefusal, and the controller's lines through this writer).
// Other streams that go to a child untouched: `legion gh`, `legion git` and `legion jj` give the
// real command they run this process's own os.Stdout and os.Stderr (what `legion jj`'s push step
// reads of a jj it runs first, and prints again, passes the rule); `legion worker-shim` gives Oh
// My Pi its os.Stderr (internal/shim) and agent-secrets the same os.Stderr for both of its
// streams; and `legion controller start` gives its foreground Oh My Pi session os.Stdin,
// os.Stdout and os.Stderr. `legion workspace-init` reads ws instead of this process (runIn).
func newRoot(stdout, stderr io.Writer, ws workspaceProcess) *cobra.Command {
	own := redact.Writer(stderr)
	root := group(&cobra.Command{
		Use:           "legion",
		Short:         "Run the Legion daemon, and reach it as its operator",
		SilenceUsage:  true,
		SilenceErrors: true,
	},
		newVersionCommand(stdout),
		group(&cobra.Command{Use: "config", Short: "read legion.yaml: check it as a start would, or print its key table"},
			newConfigCheckCommand(stdout, own),
			newConfigReferenceCommand(stdout, own),
		),
		newStartCommand(own),
		newStopCommand(stdout, own),
		newStateCommand(stdout, own),
		newLegionsCommand(stdout, own),
		newStatusCommand(stdout, own),
		group(&cobra.Command{Use: "issue", Short: "the operator's hand on an issue's Dispatch status"},
			newIssueStatusCommand(stdout, own),
		),
		newRestartCommand(own),
		newClaimsCommand(stdout, own),
		newIntakeCommand(stdout, own),
		newProjectCommand(stdout, own),
		group(&cobra.Command{Use: "sessions", Short: "copy recorded session files into Oh My Pi's session database (runtime.kubernetes.session_store postgres)"},
			newSessionsImportCommand(stdout, own),
		),
		newProbeBootCommand(stdout, own),
		newMembersBackCommand(stdout, own),
		group(&cobra.Command{Use: "controller", Short: "start the operator's controller, the interactive Oh My Pi session an operator talks to"},
			newControllerStartCommand(own, stderr),
		),
		newWorkerShimCommand(stdout, own),
		newLauncherCommand(stdout, stderr),
		newClaudeBridgeCommand(stdout, stderr),
		newModelTokenCommand(stdout, own),
		newProbeImageCommand(stdout, own),
		newWorkspaceInitCommand(stdout, own, ws),
		passThrough("git-credential", "git's credential helper in every shared clone: answers git with the token of the owner the repository's path names, or of a fork's parent's or source's owner where the path's owner has no gh directory (git runs it, never a person)", runGitCredential, stdout, own),
		passThrough("git", "the commit half of a pod's git and the git first on a tmux pane's PATH: adds a git commit's Omp-Session and Legion-Role trailers and runs the real git (a git wrapper runs it, never a person)", runGit, stdout, own),
		passThrough("jj", "the jj first on a tmux pane's PATH and the push half of a pod's jj: sets the author of a commit a push sends from its Legion-Role trailer, where its rules let it (docs/deployment.md), then runs the real jj (a jj wrapper runs it, never a person)", runJJ, stdout, own),
		passThrough("gh", "the gh first on a pane's or pod's PATH: runs the real gh with the gh directory of the owner the command names (a pane runs it, never a person)", runGh, stdout, own),
		newLeakCheckCommand(own),
	)
	root.SetOut(stdout)
	root.SetErr(own)
	// cobra's completion command runs nothing of its own, so a word naming none of its shells
	// would print its help and exit 0: it is a group like every other. Execute adds no second one.
	// Each shell's own NoArgs returns a plain error, which exits 1: a word after a shell is a
	// usage error, as after any command that takes none.
	root.InitDefaultCompletionCmd()
	for _, command := range root.Commands() {
		if command.Name() == "completion" {
			group(command)
			for _, shell := range command.Commands() {
				shell.Args = noArguments
			}
		}
	}
	root.SetHelpCommand(newHelpCommand())
	return root
}

// newHelpCommand is `legion help [command]`, cobra's own but for a topic it cannot find: cobra
// resolves every word under a command with an Args of its own to that command, so `help nope`
// printed the root's help and exited 0. Here a word that names no command is a usage error.
func newHelpCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		Long:  "Help provides help for any command in the application.\nSimply type legion help [path to command] for full details.",
		Args: func(cmd *cobra.Command, args []string) error {
			if target, rest, err := cmd.Root().Find(args); err != nil || target == nil || len(rest) > 0 {
				return usageError{fmt.Errorf("unknown help topic %q", strings.Join(args, " "))}
			}
			return nil
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
			target, _, err := cmd.Root().Find(args)
			if err != nil || target == nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			var names []cobra.Completion
			for _, child := range target.Commands() {
				if child.IsAvailableCommand() && strings.HasPrefix(child.Name(), toComplete) {
					names = append(names, cobra.CompletionWithDesc(child.Name(), child.Short))
				}
			}
			return names, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _, _ := cmd.Root().Find(args)
			target.InitDefaultHelpFlag()
			return target.Help()
		},
	}
}

// run is `legion` on argv, its exit status what the command answered. A usage error, cobra's or a
// command's own, exits 2 and names the command and the help that says how to call it; an old form
// of a command that moved exits 2 and names the new one (movedForm) before cobra reads anything.
// A build whose contract registry cannot be built runs no command at all, and is refused before
// the command tree is made, since some flags' defaults read this build's contract: its stamp does
// not name exactly the entries it registers, so its contract numbers would name other entries
// than every other build's, and it exits 1 naming each difference (api.ContractRegistryError).
// What run prints itself goes to the root's stderr, through the URL rule as every command's does
// (newRoot), and so does that refusal.
func run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	return runIn(ctx, argv, stdout, stderr, processWorkspace())
}

// runIn is run whose `legion workspace-init` reads ws (its environment, clock and runner) instead
// of this process's own: what a test runs the command with inside its test process, so the run
// changes nothing the process's other tests share.
func runIn(ctx context.Context, argv []string, stdout, stderr io.Writer, ws workspaceProcess) int {
	args := argv[1:]
	if err := api.ContractRegistryError(); err != nil {
		fmt.Fprintf(redact.Writer(stderr), "legion: %v\n", err)
		return 1
	}
	root := newRoot(stdout, stderr, ws)
	if message, moved := movedForm(args); moved {
		fmt.Fprintln(root.ErrOrStderr(), message)
		return 2
	}
	root.SetArgs(args)
	cmd, err := root.ExecuteContextC(ctx)
	var code exitCode
	var usage usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	case errors.As(err, &usage):
		return refuse(cmd, "%v", usage.error)
	}
	fmt.Fprintf(root.ErrOrStderr(), "%s: %v\n", cmd.CommandPath(), err)
	return 1
}

// movedForm answers the refusal of args when they are one of the three forms the command tree
// moved, and false for anything else. Each is read as the form was before it moved, so the call
// that used to run is the one refused: `start` given --check-config (Go's flag package also read
// it with one dash), `config-reference` in any form, and `status` whose first two words are not
// flags, which is how the old `legion status` told an issue's status from a team's.
func movedForm(args []string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	switch args[0] {
	case "start":
		for _, arg := range args[1:] {
			if arg == "--" {
				break
			}
			if name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "="); strings.HasPrefix(arg, "-") && name == "check-config" {
				return `legion start: --check-config is now "legion config check"`, true
			}
		}
	case "config-reference":
		return `legion config-reference: config-reference is now "legion config reference"`, true
	case "status":
		if len(args) >= 3 && !strings.HasPrefix(args[1], "-") && !strings.HasPrefix(args[2], "-") {
			return fmt.Sprintf(`legion status: setting an issue's status is now "legion issue status %s %s"`, args[1], args[2]), true
		}
	}
	return "", false
}

// exitCode ends a command with that status, once the command has printed what it had to say.
type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit status %d", int(c)) }

// usageError is a call the command line cannot run as written: a flag or a word it does not know,
// or a command it does not have. run prints it (refuse) and exits 2.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

// refuse prints a usage error of cmd, as one line naming the command and one naming its help, and
// answers 2, the status of a usage error.
func refuse(cmd *cobra.Command, format string, a ...any) int {
	path := cmd.CommandPath()
	fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s\nRun '%s --help' for usage.\n", path, fmt.Sprintf(format, a...), path)
	return 2
}

// action is a command's body once cobra has read its flags: it runs on the words left and answers
// the command's exit status, having printed whatever it reports.
type action func(ctx context.Context, args []string) int

// runs is the RunE of a command whose body is do.
func runs(do action) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if code := do(cmd.Context(), args); code != 0 {
			return exitCode(code)
		}
		return nil
	}
}

// group is cmd as a command that holds children: a word naming none of them is a usage error with
// cobra's suggestions (wordFirst), and the group with no word prints its usage on stderr and
// exits 2. cobra would answer both with the group's help and exit 0, and its suggestions read
// SuggestionsMinimumDistance only where cobra itself looks a word up.
func group(cmd *cobra.Command, children ...*cobra.Command) *cobra.Command {
	wordFirst(cmd)
	cmd.SuggestionsMinimumDistance = 2
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		fmt.Fprint(cmd.ErrOrStderr(), cmd.UsageString())
		return exitCode(2)
	}
	cmd.AddCommand(children...)
	return cmd
}

// wordFirst makes cmd, whose words are its commands' names alone, refuse a word that names none of
// them (commandNamed), even when a flag that is not cmd's follows the word: cobra reads cmd's flags
// before its words, and `legion workspace-init init --issue X` or `legion launcher role-inix
// --generation 1` would otherwise be refused for the flag. Every other flag error of cmd, and of
// each command under it that sets none of its own, is a usage error.
func wordFirst(cmd *cobra.Command) {
	cmd.Args = commandNamed
	cmd.SetFlagErrorFunc(func(failed *cobra.Command, err error) error {
		if words := failed.Flags().Args(); failed == cmd && len(words) > 0 {
			return commandNamed(failed, words)
		}
		return usageError{err}
	})
}

// commandNamed refuses a word that names none of cmd's commands, suggesting the ones it may have
// meant as cobra does: "Did you mean this?", then one name a line.
func commandNamed(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	err := fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
	if names := cmd.SuggestionsFor(args[0]); len(names) > 0 {
		err = fmt.Errorf("%w\n\nDid you mean this?\n\t%s", err, strings.Join(names, "\n\t"))
	}
	return usageError{err}
}

// noArguments refuses any word: the command takes its flags alone.
func noArguments(_ *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageError{fmt.Errorf("unexpected argument %q", args[0])}
	}
	return nil
}

// passThrough is a hidden command that hands every word after its name to its body unread, as git
// or gh would be handed them: a wrapper of another program, whose flags are that program's.
func passThrough(name, short string, do func(ctx context.Context, args []string, stdout, stderr io.Writer) int, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:                name + " [argument]...",
		Short:              short,
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: runs(func(ctx context.Context, args []string) int {
			return do(ctx, args, stdout, stderr)
		}),
	}
}

// afterDash is the command a wrapper runs: every word after the `--` that ends the wrapper's own
// flags. A word before it is no flag of the wrapper's and no part of the command, and is refused,
// as is a word with no `--` at all; no `--` and no word is no command (nil), which the wrapper
// refuses in its own words.
func afterDash(cmd *cobra.Command, args []string) ([]string, error) {
	dash := cmd.ArgsLenAtDash()
	switch {
	case dash < 0 && len(args) == 0:
		return nil, nil
	case dash < 0:
		return nil, fmt.Errorf("unexpected argument %q: the command to run follows --", args[0])
	case dash > 0:
		return nil, fmt.Errorf("unexpected argument %q before --", args[0])
	}
	return args, nil
}
