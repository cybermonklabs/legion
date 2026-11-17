# Deploying the Legion daemon on a shared box

This runbook covers a shared-box deployment: the daemon, its tmux server, and every pane it
launches run as the same Unix user on one machine.
This provides no filesystem boundary between the daemon and its panes.
Kubernetes deployments (`docs/kubernetes.md`) get a pod boundary instead.

The daemon gets GitHub App installation tokens from the secrets broker; the App private keys
stay in the broker.
Raw key secrets are not used.
Configure each App with `app_id` and `token_command`, as [the App section](#the-legion-implementer-and-legion-reviewer-apps) shows.

`secrets` and `agent-secrets` are two CLIs over two stores, and a name can exist in both.
`secrets` is secretsd, shipped from `sjawhar/forward`: it reads its agent tier from local sops
files and its human tier through the local secretsd daemon, which asks for a YubiKey touch.
`agent-secrets` reads the secrets broker, which is backed by a cloud Secret Manager and releases
installation tokens for `github-app` secrets.
The App configuration below uses `agent-secrets`; `provider_keys` can still read secretsd keys.

## How the daemon launcher gets tokens

Start the daemon inside an `agent-secrets` session, so every `token_command` it runs inherits that session:

```sh
export AGENT_SECRETS_URL=<the broker URL>
agent-secrets register --wait 10 --exec -- legion start …
```

The `agent-secrets` helper must be running, with the machine login a person approves through `agent-secrets machine login` ([Log a machine in](site/src/content/docs/broker/guides/log-a-machine-in.md)).
That login must be allowed to request both Apps' broker secrets.
A daemon started from a plain shell is no session: each `token_command` then fails with `NOT_A_SESSION` ([Troubleshooting](site/src/content/docs/broker/guides/troubleshooting.md)).

## The `legion-implementer` and `legion-reviewer` Apps

The `sjawhar` account owns both Apps.
The secrets broker holds them as `github-app` secrets, which release installation tokens and never the App's private key:
`LEGION_IMPLEMENT_APP` (agent tier) and `LEGION_REVIEW_APP` (human tier: each new grant waits for a person's approval in Dispatch, and a later release under the same live grant reuses it).
Its `token_command` names `--ttl 12h`, because a human-tier request that names no lifetime gets 15 minutes, and the daemon renews a lease about 40 minutes after it mints one:
without the flag every renewal would wait for a person, and with it the approver reads the twelve hours on the card and decides them.
A daemon that holds no App key uses both Apps through `token_command`, as the live proofs do for the implement App (`scripts/e2e/lib/github-apps.sh`, `app_token_command`):

```yaml
github_apps:
  implement:
    app_id: "5341240"
    token_command: "agent-secrets LEGION_IMPLEMENT_APP --reason=legion-daemon-github-app-token ${LEGION_GITHUB_OWNER:+--github-owner \"$LEGION_GITHUB_OWNER\"} ${LEGION_GITHUB_READ_ONLY:+--github-read-only} -- sh -c 'printf %s \"$LEGION_IMPLEMENT_APP\"'"
  review:
    app_id: "5341243"
    token_command: "agent-secrets LEGION_REVIEW_APP --ttl 12h --reason=legion-daemon-github-app-token ${LEGION_GITHUB_OWNER:+--github-owner \"$LEGION_GITHUB_OWNER\"} ${LEGION_GITHUB_READ_ONLY:+--github-read-only} -- sh -c 'printf %s \"$LEGION_REVIEW_APP\"'"
```

The live proofs take the review App's tokens that way no longer.
They resolve that App's key on the driver, from the operator's `LEGION_E2E_REVIEW_APP_KEY_COMMAND`, into a key file of the run's own, and give the App a `private_key_command` that reads it, while the implement App keeps the `token_command` above (`scripts/e2e/lib/github-apps.sh`, `write_github_apps`).

The daemon runs the command with `sh -c` whenever it needs a lease: at boot, and again once fewer
than 20 minutes remain on a lease it holds.
Its environment is the daemon's, less the NATS seeds and every variable whose name starts
`LEGION_GITHUB_`, with two variables the daemon sets:
`LEGION_GITHUB_OWNER`, the account whose installation token the daemon wants (empty for every
installation the command may release, which the boot's read of the App's login asks for), and
`LEGION_GITHUB_READ_ONLY`, `1` for a read-only token and empty otherwise.
The command maps each onto the flag `agent-secrets` narrows the release by.
The token reaches every repository the account's installation holds: the account chooses those
when it installs the App, and the daemon asks for no narrowing by repository.
Its stdout must be the broker's release,
`{"installations":{"<account login>":{"installation_id":<id>,"token":"ghs_…","expires_at":"<RFC 3339>"}}}`,
and the daemon takes the owner's entry, matched without regard to case.
An owner the command releases nothing for is an App not installed there.
With no key, the daemon reads each App's bot login from GraphQL's `viewer` under one of its
tokens, refuses a token whose `GET /apps/<slug>` names an id other than `app_id`, and reads whether
the App is installed on a repository from that owner's installation's repository list.
A read-only token holds what the command's read-only release gives: the broker's holds
`contents: read`, `metadata: read`, `pull_requests: read` and `actions: read`, and the first three
alone where the App's installation has not been granted Actions.
A configured key asks for that set and `issues: read` besides, so a role reads an issue through a
key-minted read-only token and not through the broker's.
A configured key reads what the installation is granted before each mint and asks for its set
cut down to it.
The daemon passes the command's stderr through as it runs, with every GitHub token in it redacted,
so `agent-secrets`' line naming the pending request appears in the daemon's log while the review
App waits for its grant.
A run is not cut off by the boot's 15-second attempt: the boot waits for it, logging that it waits,
and the next attempt joins the same run, so a grant that waits on a person's approval is one
request; `agent-secrets`' exit 75 (still waiting when its own wait ended) is retried.
`legion start --check-config` never runs it, and `installations` cannot be set beside it, since
the release names every installation.

Each App needs its own `app_id` and `token_command`, with no second credential source.
See [Rotating broker-held App keys](#rotating-broker-held-app-keys) for the rotation procedure.

## What a pane's `gh`, `git` and `jj` are

A tmux pane's `PATH` is the daemon's own with the state directory's `bin/` put first: its `legion`
launcher, its `gh`, a script that runs `legion gh`, and its `git`, a script that runs `legion git`
(below). The `jj` a pane runs is the first `jj` on the daemon's `PATH`. The daemon resolves at boot a
jj of 0.38 or later — the jj bound it refuses to boot below (`resolveTools`, naming
`LEGION_JJ_PATH`, its own override for the jj it runs) — and the real `gh` (`LEGION_GH_PATH`, or the
first `gh` on its `PATH`, refused when that is a script), which every pane is told as `LEGION_GH_PATH`. A pane's GitHub identity is
a set of files that binary reads, one directory per GitHub owner of the project, one more per owner
a configured rename moved a repository away from while the project serves the name it moved to,
and an `<owner>/<repo>` directory for each other repository the issue's recorded owner and the
owner serving the issue's repository serve: the
daemon writes gh's own `hosts.yml` (an App installation token, user `x-access-token`) and
`config.yml` under `<state_dir>/secrets/<claim>-gh/<dir>` (0700, files 0600) when it opens the
pane — in two directories, the issue's recorded owner's and the one named for the owner whose
installation serves that repository now, which are one directory unless a rename moved the issue's
repository, the role's App token for the issue's repository and no other, and in every
other directory that App's token narrowed to reading contents, pull requests, issues, Actions and metadata
over the repositories that directory names, an `<owner>/<repo>` directory's over the one repository
its own name resolves to (each token asks for what the App's installation on
that owner is granted of the set, and a permission it is granted none of is left out, named in
a warn record of the daemon's log) — names the issue owner's directory as `GH_CONFIG_DIR`
and the bundle's root as `LEGION_GH_OWNERS_DIR`, with `GH_TOKEN`, `GITHUB_TOKEN` and `GH_HOST` set
empty so nothing outranks the files, and rewrites each directory's `hosts.yml` in place every
minute from its cached leases (the log lines `tmux runtime: github credential written` and `github
credential refreshed`, with the claim, role, App, `expiresAt` and the directories written); a pane
it adopted after a restart is refreshed for the repository its claim's issue works, read from the
daemon's own state, never one read from the pane's directory. `legion gh` runs the real gh with
`GH_CONFIG_DIR` at the directory of the repository the command targets where the bundle holds one,
and of its owner otherwise, the target read as gh reads it, with pflag's rules, from the real gh's own help of
each command down to its leaf (`-R`/`--repo` before or after the command, a repository or owner
argument, before `--` or as a clone's or fork's first after it, a `github.com` URL argument — for a
`pr` command or `agent-task view` only a pull request's, since gh reads any other as a branch of
the command's own repository — every one of a slot gh's usage takes several of, `agent-task view`'s
`OWNER/REPO#N`, a `repos/<owner>/…` endpoint of `gh api` or a search endpoint's `q`, a search
query's plain `repo:`/`owner:`, the options naming a repository, an owner, a team or an issue by
URL). The command's own repository counts too whenever gh resolves it, because no argument names
it, and for `repo set-default`, which reads every remote's network, whatever names it: `GH_REPO`
for a command that reads it (`gh api`'s `{owner}` and `:owner` placeholders among them), and
otherwise every `github.com` remote and `gh-resolved` setting of the working directory, read with
gh's own URL parser, whatever the scheme. A command naming no owner, in a directory whose remotes
name none, runs at the issue owner's. A command whose last `--help` is true, and `gh --version`
alone, only print. It fails closed: a command it cannot read for certain (an option the leaf's
help does not list or names a repository with in a way it does not know, a subcommand gh does not
know, a first word gh's own help does not list, anything before the command but `-R` and
`--help`, an argument the leaf's usage has no place for or names in a way it does not know, a
search qualifier gh or GitHub could read otherwise, a gh alias or an extension, run or named as
`gh help`'s target, whose help it never reads since reading it runs them, `gh copilot` and `gh
extension exec`), one naming two owners (an option or a second repository naming one owner while
gh acts on another's, or a workspace whose remotes name two), one naming an owner the bundle
holds no directory for, every command where `PATH`'s first `gh` is not Legion's wrapper (a script
that execs `legion gh` by its absolute path), and every command under a `GH_HOST` other than
`github.com` or a set `GH_TOKEN` or `GITHUB_TOKEN` are refused with exit 2, naming the argument,
and send nothing.
Under a set `GH_TOKEN` or `GITHUB_TOKEN`, `legion gh` runs these calls and refuses every other one:
the leak guard's own two visibility reads, each exactly for the repository `PUBLIC_LEAK_CHECK_VISIBILITY_READ` names, with that token;
the shared check's own visibility read, which `legion gh` answers itself with the guard's read;
the exact `gh api rate_limit`;
and `gh auth git-credential`, which answers from the bundle.
The guard's reads use the caller's token because a push made with the token writes the repository they read,
and the bundle may hold no directory for that repository's owner.
The real gh runs with `GH_PATH`
naming the pane's wrapper, so the git gh runs itself (a clone, the fetch of a fork's parent, `pr
checkout`) authenticates through the helper gh names after it, `legion gh auth git-credential`,
which answers as `legion git-credential` does, with `credential.useHttpPath` on for `github.com`;
and with gh's update check and prompts off. The saved help its tests read is the image's gh's,
with that gh's whole command reference, every argument and every repository-naming option of
which the wrapper's tables know (`packages/daemon/cmd/legion/testdata`, with the script that
regenerates it when that pin moves; `.github/scripts/check-gh-version.sh` fails the build until
the saved help is the pin's).
The shared clone's `credential.helper` is `!legion git-credential` with `credential.useHttpPath`
on, so a pane's `git` and `jj git push` read the hosts.yml of the repository in the path git asks
about where the bundle holds its directory, the owner's otherwise, and nothing else.

The `git` in `bin/` runs `legion git --after <state_dir>/bin` (`packages/daemon/cmd/legion/git.go`).
The daemon writes it at boot, unless the first `git` on the daemon's own `PATH` is the worker
image's wrapper, `/opt/legion/bin/git`, which hands a commit to `legion git` itself. In that case
the daemon writes no `git` and removes one that an earlier boot wrote.
For a `git commit`, `legion git` adds two trailers: `Omp-Session`, with the session id that the
`dispatch` command acts as, and `Legion-Role`, with the pane's `LEGION_ROLE`. It adds each line
only when its value is valid, and only when git alone would accept the commit's message and the
two lines change nothing else.
It then runs the next `git` on `PATH` after `bin/`, a host's own git wrapper included, and it runs
every other command through that `git` unchanged.
Where a host's git wrapper runs `bin/git` again, as a wrapper that searches `PATH` for the next
`git` can, that second run goes on past each `git` the first runs ran, a script or a compiled
program, and adds nothing, so the loop ends. Where the `git` programs on `PATH` only run one
another and `bin/git`, it stops them with exit 127 and says so.
The trailers need git 2.32 or later, the first git with `git commit --trailer`.
`legion git` reads the version of one `git`: the first `git` on `PATH` after `bin/` that is a
compiled program and not a script. When that `git` is older than 2.32, the commit gets neither
line.
A host's git wrapper script can run a different `git`, and `legion git` does not read the version
of that `git`. When that different `git` is older than 2.32, it refuses the two `--trailer`
options with its unknown-option error, so the commit fails, although the same commit succeeds
without `bin/git`.
Every `git` command of a pane runs through `legion git`, so `git` can die of a signal that its
caller ignores.
The Go runtime puts its own handler over an ignore that the caller set, for each signal that it
handles except SIGHUP and SIGINT.
It does not handle SIGTSTP, SIGTTIN or SIGTTOU.
The exec sets each handled signal back to its default action.
So `git` dies of an ignored SIGTERM, SIGPIPE or SIGQUIT, for example, but an ignored SIGHUP,
SIGINT, SIGTSTP, SIGTTIN or SIGTTOU stays ignored.
`legion controller start` writes the same `git` into the `bin/` of its own state directory, by the
same rule, for the operator's controller.
The two lines record which session and which role made a commit, and they guard nothing.

A release older than the first one that writes `bin/git` leaves `<state_dir>/bin/git` in place,
and that file names a `legion` that has no `git` command, so every `git` command in a pane fails.
So before you start the older daemon, remove `<state_dir>/bin/git`. Before you run an older
`legion controller start`, remove the `bin/git` of its state directory too.

No leak guard runs in a pane's `git` or `jj`.
Legion's leak-guard wrappers and its pre-push hook are the worker image's, root-owned under
`/opt/legion` in a filesystem no role can write ([Worker image](kubernetes.md#worker-image)), and
a shared box has no such boundary: the daemon, its panes and the operator are one Unix user, so a
wrapper put first on a pane's `PATH` would be one the pane can replace, and a `core.hooksPath`
pinned for it one the pane can override.
A copy of them here would read as a guard and hold nothing.
What a pane does run the guard in is `legion gh`, the same binary with the same rules compiled
in, which resolves the shared public leak check from the pane's own `PUBLIC_LEAK_CHECK`.
A pane can find that check no other way: `PUBLIC_LEAK_CHECK` is not a name a pane reads from the
daemon's environment, a pane's `XDG_CONFIG_HOME` is under the state directory rather than the
operator's home, and the check's installer puts its files on no `PATH` a pane has.
So the daemon resolves it instead, once per launch and in its own environment — the machine's
own `PUBLIC_LEAK_CHECK`, a `public-leak-check.sh` on its `PATH`, or the pointer file under its
configuration home — and tells the pane where it is
(`resolveLeakCheck` and `panePairs`, `packages/daemon/internal/runtime/tmux`).
An operator who points one pane at another copy still wins: `runtime.tmux.pane_env` and a spec's
own `Env` are told after the resolved one, and the last value of a name is what the pane holds.
Legion installs no check on a shared box.
The pinned install is a pod's: the daemon reads the check at the tag the worker image carries and
pushes it to the pod's launcher, which installs it ([Worker image](kubernetes.md#worker-image)).
So a box that writes to public repositories carries the check through the check's own installer,
run on the machine the daemon runs on.
A daemon that resolves none logs one line per launch naming the pane, tells that pane no
`PUBLIC_LEAK_CHECK`, and every write from it to a public repository is then refused with a setup
error naming the missing check.

The real gh is a compiled program, so when the first `gh` on the daemon's `PATH`
is a script (a `gh` wrapper of the box's own, as the devbox's login `PATH` heads with, or asdf's
shim), boot refuses it, naming it and `LEGION_GH_PATH` (`RefuseScriptGH`): run as the real gh, a
wrapper would answer in place of the role's App token. Set `LEGION_GH_PATH` to the real gh's
install path (`mise which gh` or `asdf which gh` prints it). The check reads scripts only: mise's
shim is compiled and passes, and where mise has no gh for the directory it runs the next `gh` on
`PATH`, which in a pane is the pane's own `legion gh`, so a shims directory is no real gh either.

The daemon runs tmux itself: the first `tmux` on its `PATH`, with the pane environment, whose XDG
directories are under the state directory. A mise shim, or a wrapper script that hands the call to
one, then does not read the operator's mise config as its global config. Started from a directory
under the operator's `HOME`, it finds that config as a parent directory's config with no trust
record and exits 1, and boot refuses with `scrub the private tmux server's environment: … not
trusted`. Put a real tmux binary first on the daemon's `PATH`
(`mise which tmux` prints its install path), as the live proofs do with `real_bin_path`
(`scripts/e2e/README.md`).

## What a pane's Dispatch credential is

A pane's Dispatch bearer is the daemon's own. The daemon reads `dispatch_token_file` once at boot
(`readBoot`, `packages/daemon/internal/daemon/bootreads.go`) and writes the value to
`<state_dir>/secrets/dispatch-token`, a 0600 file in a 0700 directory, with
`WriteDispatchTokenFile` (`packages/daemon/internal/runtime/secretfiles.go`, called from
`prepareTmux` in `packages/daemon/internal/daemon/daemon.go`). It then tells every pane
`DISPATCH_TOKEN_FILE` naming that file (`panePairs`,
`packages/daemon/internal/runtime/tmux/spawn.go`). The value never enters tmux's argv or a pane's
environment. So a pane holds the daemon's own credential, not one of its own.

The Envoy listener bearer reaches a pane the same way. It is a launch secret, `ENVOY_TOKEN`
(`launchSecrets`, `packages/daemon/internal/daemon/launchsecrets.go`), and the tmux runtime writes
every launch secret to a 0600 file of its own under `<state_dir>/secrets` and tells the pane the
matching `<NAME>_FILE` pointer, here `ENVOY_TOKEN_FILE` (`secretFiles`, `WriteSecretFiles` and
`panePairs`, `packages/daemon/internal/runtime/tmux/spawn.go`).

When `dispatch_token_file` holds one of the deployment's shared agent tokens
(`DISPATCH_AGENT_TOKEN`), Dispatch matches the bearer against them (`MatchesSharedAgentToken`,
`packages/envoy/internal/dispatch/auth/agent_token.go`) and reads no person, no service and no
project from it. Whatever the bearer, the acting session is the one the request body names
(`bearerSessionActor`, `packages/envoy/internal/dispatch/api/server.go`). Dispatch takes any other
bearer as a service token or a personal token (`optionalActor`, the same file), and every pane's
session then carries that token's service or owner as well. `dispatch whoami` in a pane tells the
two apart: for a shared agent token it answers
`Session <id> runs under the shared token: no owner, …`
(`packages/envoy-client/src/dispatch-commands/session.ts`). A pod's side of this is in `docs/kubernetes.md`,
"Anatomy of a Sandbox pod". A person's own session on the same box uses a personal token instead,
minted from Dispatch's Settings → Agent tokens
(`docs/site/src/content/docs/dispatch/for-agents.md`).

The agent-secrets helper holds a credential of its own: the machine login a person approves for
`agent-secrets machine login`. The helper spends that credential on the broker's own routes
(`packages/envoy/internal/broker/helper/broker.go`) and asks the broker for no Dispatch token. The
broker can exchange such a login's access token for one (`docs/kubernetes.md`, "The Dispatch
identity the broker can issue"), and nothing on this box does that at this release.
`dispatch://INFRA-267` is where the daemon and its workers get Dispatch tokens of their own, a dev
box session writing under an approved machine login included, and `dispatch://INFRA-264` is where
Dispatch accepts a broker token.

## Phase workers on Claude Code

`worker_backends` in `legion.yaml` runs a role on another agent harness than Oh My Pi:

```yaml
worker_backends:
  implementer: claude
```

Each key is an issue role (`architect`, `planner`, `implementer`, `tester`, `reviewer`, `merger`)
and each value `omp` or `claude`; a role left out runs on Oh My Pi. What each harness may run is
one table, `runtime.Backends` (`packages/daemon/internal/runtime/backend.go`): Claude Code runs the
phase workers alone, since the architect's loop and the controller's are proven only on Oh My Pi,
and it runs them under either runtime — a tmux
pane here, and an issue pod under `runtime: kubernetes`, whose worker image carries the
claude-envoy plugin and Claude Code's pin, from which the pod installs and verifies the pinned
Claude Code at every launch (`docs/kubernetes.md`). The loader refuses a role on a harness that does
not run it, naming both, a role named twice, and any other value. The
controller always runs on Oh My Pi.

For such a role the pane's shim runs `legion claude-bridge -- claude -p --input-format stream-json
--output-format stream-json --verbose --replay-user-messages --thinking-display summarized
--permission-mode bypassPermissions --strict-mcp-config …`.
With `--replay-user-messages` Claude Code re-emits each user message it takes,
and the bridge answers an Envoy delivery only then,
so claude-envoy counts the delivery taken only once Claude Code has it.
With `--thinking-display summarized` it writes the text of its thinking rather than a thinking block with none,
so a worker's thinking reaches the pane and Dispatch's Agents page.
The claim's own Claude Code directory, `<state_dir>/claude/<claim token>`, is the pane's
`CLAUDE_CONFIG_DIR`: it holds the transcript, the MCP configuration the daemon writes (one server,
`legion`, running claude-envoy's `dist/envoy-channel.js` on bun) and that server's phase-reminder
state file, one per session,
and it outlives every launch of the claim. A fresh launch passes `--session-id <new id>`; a
relaunch passes `--resume <id>`, read from the transcript the claim registered, so a killed pane
comes back as the same Claude Code session. A role moved to Claude Code since its session was
recorded is refused rather than resumed, naming the Oh My Pi session file. A role moved back to Oh
My Pi gets its recorded Claude Code transcript passed to `omp --resume`, as every recorded session
file is; the pinned Oh My Pi refuses it, exiting 1 with `Cannot resume session "<file>": the
session header is missing or malformed. The file was not modified.`, and leaves its bytes as they
were, so the launch fails loudly rather than starting a fresh agent
(`packages/daemon/internal/runtime/tmux/resume_omp_test.go` holds the real binary to this).

tmux's boot resolves three things from the daemon's environment, and `legion start --check-config`
makes the same refusals:

- `LEGION_CLAUDE_PATH`, the `claude` executable, else the first `claude` on `PATH`;
- `LEGION_BUN_PATH`, the `bun` the MCP server runs on, else the first `bun` on `PATH`;
- `LEGION_CLAUDE_PLUGIN_DIR`, the absolute path of a built `packages/claude-envoy`: it must hold
  `dist/envoy-channel.js` and the generated Legion plugin's `legion/.claude-plugin/plugin.json`.
  No checkout holds either: nothing commits them. An operator gets a copy two ways. Unpack the
  published package — `npm pack @sjawhar/claude-envoy` and `tar xzf` it, whose `package/`
  directory is the plugin, built from the commit that release was cut at — or, to run a tree that
  has not been released yet, build one with `bun scripts/build.ts --out <dir>` in that package,
  into a directory outside the checkout, and point `LEGION_CLAUDE_PLUGIN_DIR` at `<dir>`. The
  build leaves the checkout's tracked files as they are, so its next `git pull` fast-forwards.

A missing one refuses the boot, naming every gap in one line. The three are tmux's alone: a pod
resolves nothing. Its bun and its claude-envoy root are the worker image's, at fixed paths
(`/usr/local/bin/bun`, `/opt/legion/claude-envoy`), which the runtime treats as image-owned so no
operator mount can hide them. Its `claude` is not the image's: the image carries Claude Code's pin,
`/opt/legion/claude-code`, and no Claude Code, since its licence grants no redistribution and the
image is public, so the pod's start script installs the pinned release at every launch into the
generation's own directory on the issue's volume, holds it to the lockfile's integrity and the
pinned version, and fails the launch, loudly, when the fetch fails or does not verify. The launch
shapes both runtimes build from those paths — the agent command, the session id and transcript
rules, the MCP document and Claude Code's own variables — are one package,
`packages/daemon/internal/claudelaunch`; the pod's side of it is in `docs/kubernetes.md`.

The daemon names no model for Claude Code under either runtime. In a pane its key reaches it the
way an Oh My Pi pane's does: `provider_keys` (for example
`ANTHROPIC_API_KEY: <secretsd key>`) is exported by the shim into `legion claude-bridge`'s
environment, which Claude Code inherits, and into every Oh My Pi pane's as well. A key meant for the
Claude Code roles alone goes in the claim's directory instead, before the claim first launches: a
`settings.json` there whose `apiKeyHelper` prints the key, as `scripts/e2e/claude-backend-tmux.sh`,
the live proof, writes it. In a pod the route is the operator's the same way: `provider_keys`
carries `ANTHROPIC_API_KEY`, and `ANTHROPIC_BASE_URL` and `ANTHROPIC_MODEL`, which are not
credentials, go in `runtime.kubernetes.pod.env` (`docs/kubernetes.md`, "Configuration").

