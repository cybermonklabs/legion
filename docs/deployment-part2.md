### How a harness plugs in

Every harness runs behind `legion worker-shim`, which speaks Oh My Pi's RPC frames to it
(`legion claude-bridge` translates them for Claude Code), so the worker stream and supervision do
not know which harness a process runs. A harness is four pieces in the daemon:

- its row in `runtime.Backends`: the roles it runs, the runtimes that launch it, the daemon prompt
  part its launches are told after their role's parts (Claude Code's is
  `internal/prompts/go/claude-mechanics.md`);
- a `Launcher` in `internal/runtime/tmux`: the command the shim runs, the extra `-e` pairs of its
  pane, and the marker its pane's command line holds;
- a boot resolver in `internal/daemon/launchers.go`, which finds what its launch needs on the host;
- the Kubernetes runtime's own branch in `internal/runtime/sandbox`, which builds the same launch
  from the worker image's paths instead: the pod's agent command, the files its launcher writes,
  the variables its start command tells the role, and the rule for the session a replacement pod
  resumes.

Boot resolves a launch for each harness `worker_backends` uses (`legion start --check-config` makes
the same refusals), and the tmux runtime refuses a launch on a harness it has no launcher for. A
pane is its recorded process only while its root process's command line, `sh -c` on the whole pane
command, holds a configured harness's marker: `omp` for Oh My Pi, ` claude-bridge -- ` for Claude
Code. A daemon with no `worker_backends` has Oh My Pi's launcher alone and checks for `omp` alone.

## The `runtime.tmux` block

`runtime: tmux` (or no `runtime` key) runs every pane with what the runtime tells it and the
allow-listed part of the daemon's environment (`HOME`, `OMP_PROFILE`, `PATH`, locale, proxies). The
mapping form adds the host's own needs:

```yaml
runtime:
  tmux:
    pane_env:                       # plain variables every pane gets, after the runtime's own
      ELAN_HOME: /opt/elan
      DISPATCH_PUBLIC_URL: https://dispatch.example.com
      LEDGER_DSN_FILE: /etc/ledger/dsn   # a _FILE pointer names a file and is allowed
    pi_config_files:                # Oh My Pi overlays, in order, in every pane's PI_CONFIG_FILES
      - /etc/legion-operator/overlay.yml
    take_over: kubernetes           # only while moving a project onto tmux (below)
provider_keys:
  ANTHROPIC_API_KEY: file:/etc/legion/providers/ANTHROPIC_API_KEY
  OPENAI_API_KEY: OPENAI_API_KEY_PROD    # a secretsd key, as before
```

- **`pane_env`** refuses `PATH` (the runtime composes it), every name the runtime sets on a pane
  (its own `LEGION_*` names, the XDG directories, the gh variables, `PI_CONFIG_FILES`,
  `LEGION_CONTROLLER`, `LEGION_SLACK_CHANNELS`), the variables it sets on every Claude Code pane
  (`CLAUDE_CONFIG_DIR`, `CLAUDE_CODE_DISABLE_CRON`), the git
  identity a launch sets (`JJ_USER`, `GIT_AUTHOR_NAME`, …), the pointers to the secrets every launch
  gets as a file of its own (`ENVOY_TOKEN_FILE`, `NATS_NKEY_SEED_FILE`), the variables that place
  the session a resume reads (`OMP_SESSION_STORAGE`, `OMP_SESSION_SQL_DSN`,
  `OMP_SESSION_SQL_DSN_FILE`, `PI_CODING_AGENT_DIR`, `PI_CONFIG_DIR`), a provider key or its
  `_FILE` pointer, any credential-shaped name (`…_TOKEN`, `…_KEY`, `…_SECRET`, `…_SEED`,
  `…_PASSWORD`, `…_PAT`, `…_GRANT`, `…_CREDENTIALS`, `PRIVATE_KEY`), and any value holding a URL
  with a password in it (`scheme://user:password@…`). A value on tmux's command line is readable by
  every process of the host, so a secret reaches a pane as a file. The loader refuses these at load
  and the runtime again at boot, naming the variable and never its value. A launch whose own
  variable or secret pointer collides with a `pane_env` name is refused.
  `OTEL_RESOURCE_ATTRIBUTES` is the one composed name `pane_env` accepts: the runtime composes
  `PI_CONFIG_FILES` from `pi_config_files` and so refuses that name in the list above, while the
  attributes are the operator's to begin and the runtime's to finish.
  The operator sets the attributes this deployment's spend is billed
  under, and each pane carries them with its own `legion.issue` and `legion.role` after them,
  so a pane's own attributes name the issue it serves. The agent keeps the last value a key is
  given, so the daemon's two pairs come last. Both keys reach the spend collector today, and
  its keep-list of six `theorem.*` attributes drops every other key, so no spend row names an
  issue yet. A row grouped by issue follows when the collector keeps them, which
  dispatch://INFRA-1835 adds to that list. A controller pane serves no issue and carries the
  role alone, and the boot gate's probe runs no role, so it carries the operator's value with its
  trailing commas and whitespace dropped and nothing added.
  The loader reads this one value for its content as well as its name: it
  refuses a member the agent's OpenTelemetry detector would throw on — one that is no single
  `key=value`, one with an empty key, one whose key or value does not percent-decode or is longer
  than 255 characters once decoded, and one naming `legion.issue` or `legion.role`, which the
  runtime writes itself — and names that member and what is wrong with it, never the rest of the
  value. One such member costs the pane every attribute of the variable, the operator's own among
  them, because the detector's caller catches the throw with an empty hand.
- **`pi_config_files`** paths resolve against `legion.yaml`'s directory and may not hold `:`.
  Legion adds no overlay of its own to the list: it writes none, so the operator's are the whole of
  it, with a role's own (`role_config_files`) after them. The
  boot plugin gate probes Oh My Pi under the same overlays and variables.
- **`role_config_files`**, a top-level key beside the block, gives one role's panes overlays no
  other role's get (`role_config_files: {implementer: [/etc/legion-operator/fallback.yml]}`, the
  controller's pane under `controller: daemon` too): such a pane's `PI_CONFIG_FILES` is
  `pi_config_files`, then the role's own in order, and nothing after them. Each
  path must be absolute; boot and `legion start --check-config` refuse one the host does not hold
  as a file the daemon's user can read, naming the role and the path, and a pane of a role the key
  leaves out is told what it was told without it. After the plugin gate passes, the boot gate runs
  `omp config get retry.modelFallback --json` under each named role's list and on each of its files
  alone, and refuses the boot when Oh My Pi exits non-zero (`boot gate: role_config_files.<role>:
  the <role>'s Oh My Pi would not start: Oh My Pi refuses <file>: error: …`), rather than letting
  every launch of that role fail. `docs/kubernetes.md` ("Operator configuration") has the rest of
  the key's rules. Every pane runs as the daemon's Unix user, so each role can read every role's
  overlay file; only loading is per role, so never put a secret in one.
- **A role's system prompt is a file.** Each launch's composed system prompt — the role's prompt
  files, the sentence that says which claim it is, the deployment's `instructions`, then each drawn
  arm's — is written to `<state_dir>/secrets/<claim token>-system-prompt.md`, kept beside the
  claim's other secret files and pruned with them, and Oh My Pi is passed that file's path as
  `--append-system-prompt <path>`, Claude Code as `--append-system-prompt-file <path>`, so no
  prompt travels as one argument and none is bounded by the 131,072 bytes Linux allows one.
- **`projects.<KEY>.memory`**, the project's own block, is one of the things that append to a
  pane's `PI_CONFIG_FILES`
  (`memory: {mode: random, config_files: [/etc/legion-memory/memory.yml], instructions: /etc/legion-daemon/memory-instructions.md}`).
  The block **is** the experiment named `memory` (`projects.<KEY>.experiments` below): the daemon
  reads it as an experiment of unit `tree` whose arms are `memory`, carrying the block's
  `config_files` and `instructions`, and `none`, carrying neither, at equal weight, with
  `on_variant: memory`.
  A project writes the block **or** `experiments.memory`, never both: the two are one experiment
  spelled twice, and a project that writes both is refused.
  The block itself is accepted and refused in no new way, so a file that passed before passes
  now, unchanged: every rule below is the rule it has always had.
  `mode` is `off`, `on` or `random`: `off` gives no tree a memory group and is what a project with
  no block gets, `on` gives every new tree one, and `random` draws a fair coin for each new tree.
  `config_files` are the Oh My Pi overlays a memory-group tree's panes name, and `instructions` the
  one markdown file the daemon reads at boot and appends to their prompt; both are absolute paths.
  The draw is made once, when the tree's lifecycle row is first created, and is kept on the tree's
  root issue record with the mode it was drawn under and the time of the draw.
  A re-admission, a new generation and the next epoch after a cleanup keep it, and a child its root
  hands over as a tree of its own takes the answer of the tree it leaves at the hand-over: that
  tree's draw verbatim where it had one, and no draw at all where that tree had been opened with
  none.
  A tree first opened before this release has no group and runs without memory, nothing draws for
  it later, and a change of mode reaches only the trees opened after it.
  A child out of a tree the daemon never opened has run under nothing and the hand-over gives it
  nothing, so its own first open draws it like any other new tree.
  Every role of a memory-group tree names the memory overlays after `pi_config_files` and the
  role's own, and before any other experiment's arms, since the block's experiment comes first
  among the project's declared experiments, and ends its system prompt with the memory
  instructions, after the deployment's own.
  The controller and every role of a no-memory tree get neither, and a block with no
  `instructions` appends nothing.
  The launch follows the record, not the mode:
  a tree the daemon drew into the memory group keeps its overlays and its instructions while the
  block still names them, whatever the mode is now, so `mode: off` stops new draws and takes
  memory from no tree already running with it.
  Taking the files away is removing them from the block.
  The loader refuses a `mode` that is no word of the three, and a mode other than `off` that names
  no `config_files`.
  It refuses a block that names `config_files` while `worker_backends` puts an issue role on
  Claude Code, which reads no `PI_CONFIG_FILES`, whatever the mode, since a drawn tree's role
  would be given overlays Claude Code never reads.
  Boot refuses an overlay no pane could read, naming the key and the file, as it does one of
  `role_config_files`, and an `instructions` file it cannot read or that holds nothing; neither
  reads the mode either.
  `legion start --check-config` makes every one of these refusals and starts nothing.
  `docs/kubernetes.md` ("Operator configuration") has what the block does under `runtime:
  kubernetes`.
- **`projects.<KEY>.experiments`**, the project's own mapping, declares the experiments it draws
  its trees and its issues into, by the name each draw records
  (`experiments: {plan-length: {mode: random, unit: issue, variants: [{name: long, config_files: [/etc/legion-experiments/plan-length.yml]}, {name: control}]}}`).
  An experiment's name is 1 to 32 characters of `a-z`, `0-9` and `-`, and the project names it
  once.
  `mode` is `off`, the default, which draws for no unit, `on`, which gives every new unit the arm
  `on_variant` names, or `random`, which draws among the arms by their weights.
  `unit` is `tree`, the default, which draws once for a whole tree and gives every issue of it the
  same arm, or `issue`, which draws for each issue on its own.
  `variants` are the arms it compares, and a mode other than `off` needs at least two of them,
  since a draw among one arm measures nothing.
  Each arm takes a `name` (the same characters, named once in its experiment), a `weight` (a whole
  number above zero, `1` where the arm names none), `config_files`, the Oh My Pi overlays a pane
  of a unit drawn into it names, and `instructions`, one markdown file the daemon reads at boot
  and appends to that pane's prompt.
  Both file keys are absolute paths, and neither is required: the arm that names no file at all is
  the control the others are measured against.
  **The order a pane's `PI_CONFIG_FILES` names them in** is the operator's files
  (`runtime.tmux.pi_config_files`), then the pane's own role's (`role_config_files`), then the
  files of each arm the pane's unit was drawn into, in the order the project's file declares the
  experiments, so an arm's file outranks both.
  The `memory` block's own arm comes first among them, since the block's experiment comes before
  the `experiments` mapping's own.
  The system prompt ends with each such arm's `instructions` in that same order, after the
  deployment's own.
  The draw is made once, when the unit is opened — a tree's when its lifecycle row is first
  created, an issue's when the daemon first records that issue — and is recorded with the arm, the
  mode in force and the instant.
  The launch follows the record, not the mode: a unit drawn into an arm keeps that arm's files
  while the project's file still names the arm, whatever the mode is now, and taking the files
  away is taking the arm out of the file.
  The controller runs in no tree and no issue, so it is in no experiment and gets no arm's files.
  The loader refuses a `mode` that is no word of the three, a `unit` that is neither word, a block
  that lists no variant, a mode other than `off` over one variant, a `mode: on` that names no
  `on_variant`, an `on_variant` that is no arm of its experiment, a name or an arm name outside
  those characters, a name or an arm name written twice, a `weight` that is not a whole number
  above zero, and an arm that names `config_files` while `worker_backends` puts an issue role on
  Claude Code, which reads no `PI_CONFIG_FILES`.
  Boot refuses an arm's overlay no pane could read and an arm's `instructions` file it cannot read
  or that holds nothing.
  `legion start --check-config` makes every one of these refusals and starts nothing.
  `legion config-reference --json` prints every key of the block with its default and what it
  does.
  `docs/kubernetes.md` ("Operator configuration") has what the block does under `runtime:
  kubernetes`, where an arm's newly declared overlay file replaces every live issue pod.
- **`projects.<KEY>.commit_issue_trailer_repos`**, the project's own list, names the repositories
  whose commits carry their issue key
  (`commit_issue_trailer_repos: [acme/widgets]`).
  A pane working one of them is told that key, and its jj appends a `Dispatch: <KEY>` line to
  every commit the pane makes in its own workspace, after the `Omp-Session:` and `Legion-Role:`
  lines every pane's commits carry, so a squash landing that branch carries the issue key in its
  message. A `git commit` gets the session and role lines from the pane's `git`, and no issue line.
  A repository the key names must be one Dispatch maps to the project (Settings → Repositories →
  Projects): the mapping is what the project serves, and this key only selects among it, so a name
  the project does not serve stamps nothing and boot says so once, naming the name and the
  project.
  A pane of a repository the key does not name is told no key and its commits carry the session
  and role lines alone, as does a commit the pane's shell makes in any other jj repository.
  The variable the pane is told is the runtime's own, so a spec, a `pane_env` and a
  `runtime.kubernetes.pod.env` are each refused for setting it.
  The loader refuses a name that is no `owner/name`, and a repository the list names twice,
  compared without case and through `repository_renames`.
- **`take_over: kubernetes`** lets this daemon take over the claims a daemon running the project
  under `runtime: kubernetes` recorded on the same store ([Moving a project onto
  tmux](#moving-a-project-onto-tmux)). `kubernetes` is the only accepted value. Without the key, a
  claim whose recorded process is a pod is refused, naming the claim and the key, and the boot does
  not start: two daemons must never drive one store's claims at once.
- **`provider_keys`** values of the form `file:/abs/path` are read once at boot, as the daemon,
  into the same daemon-held 0600 files a secretsd key lands in. The file is read by the rules every
  other secret file the daemon reads follows: one that cannot be read, is not a regular file, holds
  nothing, or is readable by its group or others refuses the boot, naming the variable and the
  file. Kubernetes refuses the form: a file on the daemon's disk reaches no pod.
- **`controller: daemon`** works under tmux too: the daemon launches the controller in a pane of
  its own window, `controller`, working in `<state_dir>/controller`, told `LEGION_CONTROLLER=1` and
  an empty `LEGION_SLACK_CHANNELS`, and supervises it like a root architect. It needs
  `dispatch_url`. A project with no GitHub owner to mint for runs it with gh acting as nobody, its
  gh directory empty.

## How workspaces are provisioned on the host

Each repository has one shared jj clone at `<state_dir>/repos/github.com/<owner>/<repo>`, and each
issue a jj workspace of it at `<state_dir>/workspaces/<owner>/<repo>/<issue, lowercased>`, on the
bookmark `legion/<ISSUE>`. The first provision of a repository clones it with the implement App's
installation token, handed to git through a one-shot 0600 credential helper under the state
directory. Every launch first makes sure its issue's workspace is there: a claim resumed,
relaunched or recovered, as well as a new one, is provisioned when its directory is missing or its
checkout never finished (it holds no sparse pattern, read with `jj sparse list
--ignore-working-copy`; a workspace an agent narrowed keeps its patterns), checking out
`legion/<ISSUE>` when GitHub has it. A launch waits for a provision or removal of the same issue
that is under way.

Provisions of different issues run side by side. Everything a provision does to the shared clone
runs under the clone's one store lock: a fetch of the default branch and the issue's branch alone,
the workspace's registration with no files (`jj workspace add --sparse-patterns empty`), its
bookmark, and the clone's git configuration. The checkout of the working copy (`jj sparse reset`
in the new workspace) writes only that workspace and runs with the lock released; at most 16
checkouts run at once, and a provision waiting on its repository's store lock holds no place
among them. A workspace whose checkout never finished is checked out by its next provision, which
its next launch runs. A removal takes the issue's turn and holds the store lock only for its jj
commands on the clone and the move of the workspace aside to `<dir>.removing`; the directory is
deleted after the lock is released, and a `<dir>.removing` a crash left is cleared by the issue's
next removal.

A store may be made before the daemon's first boot, with the same jj the daemon uses, as the
daemon's user: a colocated `jj git clone https://github.com/<owner>/<repo>` into the clone path,
then `jj -R <clone> --ignore-working-copy config set --repo git.abandon-unreachable-commits false`.
A directory there without `.jj` is removed and cloned again; one without `.git` is not a store.
Boot rewrites every store's credential helper to `legion git-credential`.

## Moving a project onto tmux

A tmux daemon can take over the store of a daemon that ran the project under `runtime: kubernetes`,
when its `legion.yaml` sets `runtime.tmux.take_over: kubernetes`. Without that key it refuses every
claim whose recorded process is a pod, and does not start. Nothing else stops two daemons from
driving one store at once — the instance lock is a file on each machine, and claim records are
written by whichever daemon wrote last — so the order matters:

1. Stop the Kubernetes daemon. Its issue pods outlive it: push every issue's unpushed work, then
   delete the project's Sandboxes (`kubectl -n <namespace> delete sandbox -l
   'legion.dev/project=<project>'`), which takes each pod and each pod's disk, and wait until none
   of the project's Sandboxes or pods is left. A pod still running beside the tmux daemon is a
   second agent on the same claim and branch.
2. Put each claim's session file where its pane's Oh My Pi keeps sessions, in a folder one level
   down, under its recorded file name (`<timestamp>_<session id>.jsonl`): either at the recorded
   path itself, or in the pane's own sessions directory. That directory is
   `<HOME>/.omp/agent/sessions`, or `<HOME>/.omp/profiles/<profile>/agent/sessions` when the
   daemon's environment or `pane_env` sets `OMP_PROFILE` or `PI_PROFILE`, or
   `<state_dir>/home/.local/share/omp[/profiles/<profile>]/sessions` when that directory already
   exists; a profile named only inside `omp_launch_prefix` is not read. A session the Kubernetes
   runtime kept in its session database (`runtime.kubernetes.session_store`) is not carried: no
   command writes its row out as a file, so its claim starts a fresh agent from the issue's pushed
   branch and committed handoffs.
3. Set `runtime.tmux.take_over: kubernetes` and boot the tmux daemon with the same Postgres. Each
   claim whose recorded process is a pod is reported gone, displaced, and relaunched in a pane,
   charged nothing, on its session when step 2 put its file in one of those places, searched by
   file name or session id. A session found nowhere starts a fresh agent, uncharged, which
   registers a session of its own; the loss is posted on the issue and told to its architect, to
   the controller and to the new agent in its own prompt. That holds whether or not `take_over` is
   set — a session no store holds is gone whichever runtime last wrote it — so what this key buys
   is the takeover of the other runtime's locators: a claim whose pod this daemon cannot see is
   resumed without waiting that process out. Each workspace is provisioned before its first launch.
4. Retry what failed to launch meanwhile, once the cause is gone: `legion claims retry --failed`
   (an architect relaunches on its session; a phase worker whose issue is held for its own role
   retries that phase).
5. Remove `take_over` once no claim can still wake on a session a pod recorded: every claim that
   was running has relaunched in a pane, and no claim the Kubernetes daemon suspended (a
   production check's checking role — the one the project's `production_check` names — waiting
   for the look its own `wake_at` asked for among them) is left to wake. From then on a resume
   over a locator of the other runtime is refused, and a tmux daemon pointed at a store a
   Kubernetes daemon still drives refuses to start rather than take its claims.

## Rotating broker-held App keys

Rotate the keys in the broker's `github-app` secrets, not on the daemon's machine:

1. In each GitHub App's settings, generate a new private key and leave the old key active.

2. Update the matching broker secret, `LEGION_IMPLEMENT_APP` or `LEGION_REVIEW_APP`, as [Store the App key](site/src/content/docs/broker/guides/github-app-for-a-dev-box.md) shows:
   its value is the JSON document `{"app_id": …, "private_key": "<the new PEM>"}`, set with `agent-secrets secret set <NAME> < app-key.json`, never the bare PEM.
   Keep the secret's `github-app` kind and its GitHub owners.

3. Open a registered shell of your own (`agent-secrets register --wait 10 --exec -- sh`; the daemon's session cannot be entered from another terminal), then run each role's `token_command` from [the App section](#the-legion-implementer-and-legion-reviewer-apps) there, with `LEGION_GITHUB_OWNER` and `LEGION_GITHUB_READ_ONLY` unset, and confirm it prints the installations' tokens (`LEGION_REVIEW_APP` waits for a person's approval).
   Then confirm a fresh worker pane's `gh auth status` succeeds with account `x-access-token`.
   The daemon needs no restart: `token_command` gets fresh tokens as its cached leases near expiry.

4. Only then revoke the old private keys in GitHub App settings.
   Revoking them before the broker can mint with the new keys would break token refresh and,
   once existing tokens expire, the panes' `gh` and `jj git push` credentials.

## Saying when the daemon is serving

`GET /readyz`, at the API port legion.yaml's `port` names (13370 by default, `defaultPort` in
`packages/daemon/internal/config/config.go`), is the daemon saying whether it serves. It carries no
token and reads no database, and what a caller sees follows the daemon's life:

- While the boot runs, no answer at all. The daemon binds its port early in the boot but serves
  HTTP only once the boot is done, so a request waits in the kernel's queue until the caller's own
  timeout runs out.

- `200 {"status":"ok"}` once the daemon serves: its runtime built, its launcher acceptor installed
  and its API serving. `503 {"status":"starting","waitingFor":"the daemon to serve"}` answers only a
  request that reaches the instant between the server starting and the latch being set.

- `503 {"status":"stopping"}` from the moment the daemon's stop begins, on a connection the server
  still holds through its drain. A new connection gets no answer, because the drain closes the
  listener at once.

It reports nothing beyond that boot's own progress, so the answer is the same under any load and
while every connection of the pool is busy. `/healthz` answers 200 on every request the API takes,
the drain's included, and says nothing about the boot, so a start script or a supervisor on this box
waits on `/readyz` instead, with a bound on each try, since a try during the boot gets no answer:
`until curl -fsS --max-time 2 http://127.0.0.1:13370/readyz; do sleep 1; done`.

A Kubernetes deployment declares it as the daemon container's readiness probe. Those manifests are
in another repository, not this one; the probe to declare on that container, and why it is a
readiness probe and never a liveness probe, are in the Kubernetes guide's Configuration section
(`docs/kubernetes.md`).

## Serving on a socket the harness holds

`legion start` binds legion.yaml's `bind` and `port` for its API. With `LEGION_LISTEN_FD=<n>` in
its environment, it serves the API on descriptor `<n>` instead, a listening TCP socket it inherited
from whatever started it, and binds nothing. A test harness opens that socket on port 0, so the
kernel chooses the port, and holds it until the daemon serves on it; no other program can take the
port in between. A shared-box deployment leaves the variable unset.

- The file's `bind` and `port` must name the socket's address: `bind` an IP literal equal to the
  socket's IP (`0.0.0.0` matches only a socket on every IPv4 interface, and a host name matches
  nothing), `port` its port. The registry entry, the default `daemon_url` and `legion state` dial
  them. A socket they do not name, or a descriptor that is not a listening TCP socket, refuses the
  start before it takes the team or opens the store, naming `LEGION_LISTEN_FD` and the setting:
  `legion start: legion.yaml: port=8081 does not match LEGION_LISTEN_FD=3, a socket listening on 127.0.0.1:8080`.
- The daemon takes the socket over before it runs anything, the configuration's token commands
  included, and serves on a close-on-exec copy, then unsets the variable: no process it starts, its
  panes and tmux server among them, holds the socket or inherits the variable.
- The boot line `legion daemon started` names the address the daemon serves on, as it does when it
  binds.
- `legion start --check-config` binds nothing and never reads the variable.
