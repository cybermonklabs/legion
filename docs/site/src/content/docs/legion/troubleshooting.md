---
title: Troubleshooting
description: The failures Legion reports, what each means, and the command that shows it.
sidebar:
  order: 5
---

Legion refuses loudly rather than carrying on in a state it cannot vouch for: a refusal names what
is wrong and, usually, what to do. This page collects the ones an operator or a user meets, grouped
by where they show up. The commands assume `legion.yaml` and the operator token are in the current
directory; `<KEY>` is an issue key such as `WIDGETS-12`.

## The daemon will not start

**Run the configuration check first.** It makes every refusal the boot would make from the file,
the environment and the files the configuration names, and starts nothing:

```sh
legion start --config legion.yaml --check-config
```

| It says | What to do |
| --- | --- |
| `runtime.kubernetes.image must be pinned by digest (@sha256:…)` | Pin the image by digest, never a tag ([The worker image](/legion/legion/running-legion/#the-worker-image)). |
| `bind 127.0.0.1 is not an address a pod can reach, …` (or the same for `daemon_url`, `envoy_url`, `dispatch_url`, a `nats_urls` entry, or `advertise_host`) | Every address handed to a pod must be one a pod reaches: the daemon host's own address, never loopback or `0.0.0.0` — except `bind`, which may be `0.0.0.0` once `advertise_host` names the stable address pods dial instead. |
| `bind 127.0.0.1 is loopback, where no pod reaches the worker stream, whatever advertise_host names; …` | With `advertise_host` set, `bind` is where the daemon listens: `0.0.0.0`, or the daemon host's own address. |
| `advertise_host must be an IP address or a DNS name, …` or `advertise_host is not used when runtime is tmux: …` | `advertise_host` is the host alone (no scheme, port, path or brackets), and only `runtime: kubernetes` reads it. |
| `<key> must be an IP address or an ASCII DNS name; got "…\u212a…"` (for `advertise_host`, `bind`, an NFS volume's `server`, or the host of `daemon_url`, `envoy_url`, `dispatch_url`, a `nats_urls` entry or `agent_secrets.url`) | The host has a character outside ASCII, printed as a `\u` or `\U` escape. Write the name in ASCII letters, digits, hyphens and dots (an internationalized name in its `xn--` form). |
| `<key> is required when runtime is kubernetes: …` | Add the key; the message says why a pod needs it. |
| `worker_stop_timeout_seconds must be at least 9 when runtime is kubernetes: …` | Raise it to 9 or more. A role's stop gives its agent 5 s to exit, drains its output for 1 s and waits at least 3 s for the CodeGraph warm-up to release its lease on the issue volume; a smaller grace would let the pod kill the shim with the lease held, and the role's relaunch would skip its own build for a minute. |
| `projects must configure <PROJECT>, the daemon's own project` | `project` must also be a key of `projects`. |
| `postgres_dsn is required (or set LEGION_POSTGRES_DSN)` | Give the daemon its database. |
| `unknown key <key>` | A typo, or a setting Legion no longer has; the message says which when it knows. `Unknown key "projects.<KEY>.repo": the project's repositories are Dispatch's mapping …` is one: map the repositories in Dispatch and delete the key ([Upgrade](/legion/legion/running-legion/#upgrade)). |
| `omp_invocation is not used when runtime is kubernetes: …` | Remove it: every pod runs the worker image's Oh My Pi. |
| `worker_backends.architect is claude (Claude Code), which runs only planner, implementer, tester, reviewer, merger: …` | Claude Code runs the phase workers alone; leave the architect on `omp`. |
| `worker_backends runs a role on claude, and boot is missing …` | Under tmux, give the daemon's environment what the message names: `LEGION_CLAUDE_PATH` and `LEGION_BUN_PATH` (or `claude` and `bun` on `PATH`), and `LEGION_CLAUDE_PLUGIN_DIR`, the absolute path of a built `packages/claude-envoy`. A Kubernetes daemon needs none of them: the worker image carries the plugin and Claude Code's pin, and each Claude role's pod installs the pinned Claude Code at launch, so `worker_backends` is accepted there with nothing else set. A launch whose install fails or does not verify fails, printing a `legion claude-code: …` line, so such a pod must reach the npm registry. |
| `role_config_files.<role> entry <file> is under no runtime.kubernetes.pod.volume_mounts mount_path: …` (or `… is not among the items volume <name> projects …`) | Under Kubernetes a role's overlay must be a file the operator's mounts carry: mount the ConfigMap holding it, or add the item. |
| `role_config_files.<role> names <file>, which the <role>'s Oh My Pi could not read: …` | Under tmux the file must exist on the host and be readable by the daemon's user, as every pane is. |
| `role_config_files.<role> …` (another refusal) | The message names the entry: a role that is not Legion's, an empty list, a relative path, a path named twice or already every role's, the controller without `controller: daemon`, or a role on `claude`. |
| `worker_stall_backstop_hours.<role> (<bound>) is below the whole stall ladder the <role>'s own plugin runs (<longest call> + <grace> + 2 × <step> = <ladder>, where the first number is …): …` | Raise that role's bound to the ladder the message names, or set `0` to switch the backstop off for that role. The daemon's backstop is there for a worker whose own stall watch is itself broken, and the daemon cannot see that watch running, so a bound below the ladder would end a turn the plugin it backs up is still recovering. The ladder is the longest one call of that worker can legitimately run, plus the grace over a deadline a call declared for itself (`LEGION_STALL_CALL_GRACE_MS`, 5 minutes), plus the two gaps between the recovery steps (`LEGION_STALL_STEP_MS`, 2 minutes each). The longest call is the larger of two numbers, and the message says which one it took: the bound a tool call no deadline judges gets (`LEGION_STALL_CALL_BOUND_MS`, by default 4 hours for an implementer and a reviewer and 1 hour for every other role and the controller, and that default again where the deployment sets `0`, which switches the worker's watch off rather than bounding its calls at zero), and the hour the host clamps a deadline a call declares for itself to ([legion.yaml](/legion/legion/running-legion/#legionyaml)). |
| `projects.<KEY>.memory.mode must be off, on, or random (got "…")` | The mode is one of three words: `off` (the default, and what a project with no `memory` block gets), `on`, or `random`. |
| `projects.<KEY>.memory is <mode> and names no config_files: …` | A mode other than `off` draws new trees into the memory group, and those trees read their overlays from that key: name at least one, or set `mode: off`. |
| `projects.<KEY>.memory names config_files, and worker_backends.<role> is claude, which reads no PI_CONFIG_FILES: …` | A block that names overlays hands them to every memory-group tree's roles whatever the mode is, and Claude Code reads no `PI_CONFIG_FILES`: put every issue role on Oh My Pi, or take the overlays out of the block. |
| `projects.<KEY>.memory.config_files entry <file> is under no runtime.kubernetes.pod.volume_mounts mount_path: …` (or `… is not among the items volume <name> projects …`) | As for a role's overlay: under Kubernetes a memory overlay must be a file the operator's mounts carry, so mount the ConfigMap holding it, or add the item. It is refused at `mode: off` too, since a tree already drawn into the memory group still names it. |
| `projects.<KEY>.memory.config_files names <file>, which a memory-group role's Oh My Pi could not read: …` | Under tmux the file must exist on the host and be readable by the daemon's user, as every pane is. |
| `projects.<KEY>.memory.config_files …` or `projects.<KEY>.memory.instructions …` (another refusal) | The message names the entry: an empty list, a relative or unclean path, a path holding `:`, which separates the overlays `PI_CONFIG_FILES` names, or a path named twice. |
| `memory instructions file <path> could not be read: …` or `memory instructions file <path> is empty` | Boot reads that file from the daemon's own disk and puts its text in every memory-group role's prompt, so give it a readable file with text in it, or take `instructions` out, which appends nothing. Boot reads it whatever the mode is, since a tree already drawn into the memory group is given it. |
| `projects.<KEY>.commit_issue_trailer_repos[<n>] must be "owner/name" (got "…")` or `projects.<KEY>.commit_issue_trailer_repos names <owner>/<name> twice` | Each entry is one repository, written `owner/name`, and one repository is named once: names are compared as GitHub compares them, without case, so a second spelling of one name is the file saying the same thing twice. |
| `projects.<KEY>.commit_issue_trailer_repos[<n>] (<owner>/<name>) and projects.<KEY>.commit_issue_trailer_repos[<m>] (<owner>/<name>) name one repository: repository_renames makes … one repository, so the list names it twice; name it once` | A configured rename makes the two entries select the same repository. Keep the one the project uses now and delete the other. |
| `<PROJECT> is already running (pid <n>)` (from `legion start` itself) | A daemon for this project is already registered on the machine: `legion status <PROJECT>`, `legion legions`. |
| `capability <name> is open: <detail>; to record a decision, add to legion.yaml: capabilities.decided.<name>: "<reason>"` (after the `Config OK` line, exit 0) | A report, not a refusal: the deployment leaves a worker capability open. The check names the rows the file alone decides — `resource-limits` while a role lacks CPU and memory in both requests and limits, `secrets` while no broker is configured — and the daemon logs the same line at boot for every open row, `model-fallback` included when the probe read `retry.modelFallback` false under your overlay or could not read it at all (the probe's OK line marks that `model-fallback=unread`, the probe pod's log says why), and `pool-capacity` when the image probe pod was Unschedulable before it scheduled (the detail counts the attempts that waited and quotes the scheduler's reason). Close the gap, or add the `capabilities.decided.<name>: "<reason>"` line it prints to record your decision. `legion state --json` lists every row under `capabilities`, each deployment row carrying `source` (`configuration` or `boot`), and the controller's daily report names each open one. In the daemon's log alone (the check prints no such row), a line without the `; to record a decision` tail — `capability <name> is open: pod <sandbox>/<container> (<role>, <issue>): <fact>`, or `pane <window>:<pane>` under tmux — is a live row (`subagents`, `web-search`, `mcp`, `repository-extensions`, `dispatch-envoy-tools`, `github`) that a running session's own check at its start failed: the daemon logs the fact alone, with the claim, role, issue and locator as the line's attributes, as the session reports ready, then this pod-naming form at boot and at the first controller tick after the set of open rows changed, and `legion state` shows the row `open` with that detail, naming each failing session. The fact is what the session measured inside its Oh My Pi (`web_search is not a registered tool`, `not connected: <server> (<error>)`, `agents the prompts dispatch that the task tool does not resolve: <names>`, `not discovered: <paths>`, `Envoy tools missing: <names>`, `dispatch read failed: <stderr>`, `gh viewer login failed: <stderr>`, or `did not finish within 8 s`). No `capabilities.decided` line applies: fix what the fact names in the session's environment, and the row heals when that process is relaunched with the gap closed or reports anew at its next boot — a session that booted during a Dispatch, GitHub or search-provider outage reads open until then. The session's own report is under `issues.<KEY>.architect.capabilities` or `issues.<KEY>.workers.<role>.claim.capabilities` in `legion state --json`. |

**At boot, after the check passes**, the daemon reads the project's repositories and checks the
cluster and the image:

- **No repository mapped.** `project <PROJECT> has no repository mapped in Dispatch (Settings →
  Repositories → Projects)`: map the project's repositories in Dispatch, one of them its default.
- **A repository is left out.** A mapped repository missing either App does not stop the boot,
  even when it is the project's only one: the log's `legion workflow boot stage` line with
  `stage=repositories` lists it under `refused`, `legion state --json | jq .repositories.refused`
  names it with the App it lacks, and its issues are not admitted, each saying so, until the App
  is installed there and the issue moves to `todo` again.
  A refusal that reads `the <role> App's installation on <owner>/<repo> could not be read: …;
  Legion reads it again` is GitHub not answering, not a missing App: the daemon reads the mapping
  again on its own, 30 s doubling to 10 min, and serves the repository once GitHub answers; an
  issue handed to Legion on it, before the boot or after, has nothing said on it, and the daemon
  recovers it from Dispatch's listing once GitHub answers, with nothing to move. A
  `child-release-stalled` notice that names such a repository says which parent to release first:
  the recovery records no child under a parent that is not `todo`.
- **A stamped repository the project does not serve.** `workflow: commit_issue_trailer_repos names
  a repository this project does not serve, so no pane stamps a commit with the issue key for it`,
  with the repository and the project: `projects.<KEY>.commit_issue_trailer_repos` only selects
  among the repositories Dispatch maps to the project, so this name selects nothing. It is a
  warning, not a refusal — the boot's check reads no Dispatch, and a repository mapped the day
  after the file is written is no fault of the file. Map the repository to the project in
  Dispatch, or take the name out.
- **Records with no repository.** `<n> issue records have no repository and Dispatch resolves none
  for them (<KEY>, …): set the project's default repository in Dispatch …`: the first boot of this
  release records each issue it already runs on the repository Dispatch resolves for it, and these
  resolve to none. Set the project's default repository in Dispatch, or each issue's own, and start
  the daemon again.
- **A record whose work is elsewhere.** `fill the repository of <KEY>: Dispatch resolves it to
  <owner/name>, but its pull request #<n> is on <owner/name>` (or `its workspace <dir> is in
  <owner/name>`) `: set the issue's repository in Dispatch to … and restart`: the issue worked in
  that repository, but its own repository or an ancestor's, set in Dispatch since, resolves it
  elsewhere. Set the issue's repository in Dispatch to the one the message names, with the command
  it names, and start the daemon again.
- **Agent Sandbox is missing.** The refusal names `the Sandbox CRD sandboxes.agents.x-k8s.io` or
  `the Agent Sandbox controller Deployment agent-sandbox-system/agent-sandbox-controller`, and
  whether the API answered 404 or 403. A 403 means the daemon's identity lacks the get the
  [RBAC list](/legion/legion/running-legion/#the-cluster) names.

  ```sh
  kubectl get crd sandboxes.agents.x-k8s.io
  kubectl -n agent-sandbox-system get deployment agent-sandbox-controller
  ```

- **The image probe refuses.** The daemon runs the worker image's launch probes in a probe Sandbox,
  `legion-probe-<project>-<digest prefix>`, and quotes the probe's log. The causes it names:
  - the image's plugin speaks another daemon contract than this binary: run the `legion` binary
    from the same commit as the image ([Upgrade](/legion/legion/running-legion/#upgrade));
  - the registry does not have the image (`… failed its probe: the registry does not have image
    <image>: …`, quoting the kubelet's `rpc error: code = NotFound … not found`): the configured
    digest was never pushed, or was pushed to another repository. Point the daemon at a digest the
    registry holds (`docker manifest inspect <image>` answers for one it has); if it does, and your
    nodes pull through a mirror, the mirror lacks it. The probe refuses this once the kubelet has
    met that answer on two pulls, within about half a minute; a pull refused for credentials, a
    network failure or a rate limit is retried instead;
  - an agent's model does not resolve (`task agent <name> … role <role> is not configured`), or its
    key does not work: name the role in your overlay's `modelRoles`, re-run `apply.sh`, and check
    the credential your `models.yml` reads;
  - the probe pod cannot mount the providers Secret (`… cannot mount the providers Secret
    legion-<project>-providers …`): create the Secret with every key `provider_keys` names;
  - the probe pod cannot mount a volume of your `runtime.kubernetes.pod` block that a `volume_mounts`
    entry mounts (`… cannot mount volume "<name>", which runtime.kubernetes.pod.volumes gives it from
    ConfigMap "<name>" …`, or `Secret "<name>"`, quoting the kubelet's `configmap "<name>" not found`,
    `secret "<name>" not found` or the key it could not find): create the object in the daemon's
    namespace, or fix the name or key in the block. The daemon refuses on the kubelet's first
    `FailedMount` (a volume no container mounts is never attempted, so never refused), minutes into
    the boot, and `legion probe-boot` shows the same refusal before a restart; a container waiting
    `CreateContainerConfigError` for an object its environment reads (`… cannot configure its
    container probe (waiting: CreateContainerConfigError) …`) has the same remedy;
  - the probe pod cannot mount a share of your `runtime.kubernetes.pod` block (`… cannot mount volume
    "<name>", which runtime.kubernetes.pod.volumes gives it from PersistentVolumeClaim "<claim>" …`
    or `… from NFS export <server>:<path> …`): the claim is missing from the daemon's namespace
    (`persistentvolumeclaim "<claim>" not found`), or the node's mount failed (`mount failed: …`,
    such as `mount.nfs: Connection timed out` for a server the nodes cannot reach, or `access denied
    by server` for an export it does not have). Create the claim, or check that the nodes reach the
    server and the export exists. `… could not start its container probe over its mounts, which
    gVisor could not read …` is a share the node mounted but cannot read: check the server's export
    and its NFS version. With two or more claims the refusal reads `… cannot mount PersistentVolume
    "<pv>", bound to one of the claims runtime.kubernetes.pod.volumes gives volumes <a>, <b> from …`:
    `kubectl get pv <pv> -o jsonpath='{.spec.claimRef.name}'` names the claim. A CSI driver's
    `Aborted`, `DeadlineExceeded`, `Unavailable`, `ResourceExhausted` or `Canceled` is left to the probe's
    budget while the kubelet retries it; a claim must be `ReadWriteMany`, since every issue pod mounts it from its own node;
  - the image names another NATS user than the daemon's seed: put the agents' seed in the providers
    Secret's `NATS_NKEY_SEED` key;
  - `capability <name> is missing: <detail>` in the quoted log: the image lacks a tool the capability
    check looks for (`chromium`, a language server, `go`, …), or an operator pod env such as
    `PUPPETEER_EXECUTABLE_PATH` names one that does not run. Rebuild the image from a commit whose
    Dockerfile carries it, or fix the variable; the worker-image workflow fails the same way on an
    image that lacks one;
  - `Succeeded without checking the capability list (its legion CLI predates the check)`: the image's
    `legion` is older than this daemon's capability check. Build the image from this daemon's
    commit.
  - `… refused role <role>'s Oh My Pi overlays: <file> cannot be read: …` (or `Oh My Pi refuses
    <file>: error: …`, quoting Oh My Pi's own error): a file `role_config_files` names for that
    role is not in the mount you gave it, or Oh My Pi cannot parse it. Add the key to the ConfigMap
    the mount serves (an `items` entry too, when the volume lists them), or fix the file or the
    path; `its legion CLI has no -role-config-files` is an image older than the key, built before
    this daemon's commit. Under tmux the boot gate refuses the same way: `boot gate:
    role_config_files.<role>: the <role>'s Oh My Pi would not start: Oh My Pi refuses <file>: …`.

  A model key that fails at boot refuses the boot even when the cause is a passing network blip,
  since the probe cannot tell the two apart. Start the daemon again once the cause is gone.

## The daemon stops

On SIGTERM (`legion stop`, a rollout, a node drain) the daemon logs `legion daemon stopping`, with
`deciding` naming each claim whose decision is in flight and its event. It cuts short every
relaunch and suspension it started itself and, once it has recorded its boot, the boot steps still
running. A request in flight from the operator or an agent gets up to 8 seconds to finish, so a
suspension or stop whose agent is already exiting is recorded. An operator request that would
change a claim and that the daemon accepted before the stop began, but had not started deciding,
is answered `503` (`<request> refused: the daemon is stopping; ask again once it is back`); one
sent once the stop has begun gets the same 503, or finds no daemon listening once the daemon has
closed its API listener. Repeat either once the daemon is back. It logs `legion daemon stopped`
once it has recorded the boot's end. It waits at most 10 seconds for its own work before it records
that, so it stops well inside a pod's termination grace. It ends no agent: the next boot re-adopts
every pod or pane still running and relaunches each launch the stop cut short.

A signal that comes before the boot is recorded ends the boot step under way only when that step
is the plugin gate, which runs first, the GitHub App token mint, the image probe, or the NATS and
Dispatch readiness checks; the daemon then stops without recording a boot. The cluster check,
opening and migrating the store, building the runtime and recording the boot are not cut short:
each runs to its end, with at most 30 seconds between one of the steps that end at the signal and
the next, and the daemon stops at the next such step, or once its boot is recorded, which it then
stamps.

- **`legion daemon stopped draining the API`.** A request was still in flight after those 8
  seconds. The daemon cut it short, the caller got an error, and the next boot takes its claim up;
  repeat the request once the daemon is back.
- **`legion daemon stopped waiting for its work`.** Some of the daemon's work had not ended when that
  wait ran out, which takes a call into a process that does not answer; `deciding` names each claim
  whose decision was still running, with its event. The daemon stopped anyway, and the next boot
  takes those claims up.

## The controller

- **The daemon logs `controller not registered; run legion controller start`.** Nobody is running
  the controller. Start it ([Start the controller](/legion/legion/running-legion/#start-the-controller)).
- **The daemon logs `controller not registered; run legion controller start only under controller:
  operator; this daemon launches its own controller and relaunches it`.** The controller the daemon
  launches has not registered, or its session is gone. The line's `claimState` says where its claim
  is: `launching` while a launch is in flight, `failed` or `retired` while the daemon waits to retry
  it. For a launch that keeps failing, see "The daemon-launched controller keeps failing" below.
- **`legion controller start` is refused: `this daemon launches the project's controller itself
  (controller: daemon), so legion controller start has none to start`.** The daemon runs its own
  controller as a pod; reach it through Dispatch instead.
- **The daemon-launched controller keeps failing.** `legion claims list` shows its claim,
  `legion-<project>-controller`, and the daemon logs why each launch failed and when it retries a
  failed one. `kubectl -n <namespace> describe pod legion-<project>-controller` and its logs show the
  pod's own side. If the stopped controller's session was lost with its volume or wedged by a
  provider refusal, take its recorded session from the claim and run `legion claims forget-session
  --claim legion-<project>-controller --session <recorded session id> --operator-token-file <file>`.
  The daemon clears the claim's matching session record only; it does not read or change the session
  database, so its old row remains. Its keeper then retries the claim with a fresh session.
- **The daemon refuses to boot: `stop legion-<project>-controller, the controller an earlier boot
  under controller: daemon launched, since this daemon leaves the controller to its operator
  (controller: operator): …`.** `legion.yaml` was switched back to `controller: operator`, and the
  daemon could not stop the pod it launched before; the end of the line says why (most often the
  cluster refused the Sandbox's deletion). The refusal leaves the pod and its registration as they
  were. Fix the cause and start the daemon again, or set `controller: daemon` back, which re-adopts
  that controller with its registration, grants and wakes still working.
- **The daemon refuses to boot: `end the registration of legion-<project>-controller, the controller
  an earlier boot under controller: daemon launched, since this daemon leaves the controller to its
  operator (controller: operator): …` (or `read the controller record to end the registration of
  …`).** The daemon stopped its controller's pod but could not write the controller record; the end
  of the line is the store's error. Start the daemon again once Postgres answers: the next boot finds
  the claim retired and ends the registration before it serves anything. The record names the
  stopped pod's session until then, but no refused boot serves the state; it shows as the state's
  `controllerLocator` only if you set `controller: daemon` back before an operator boot succeeds.
  Nothing can act as that session, since the process that held its registration secret ended with
  the pod; the pod's Secret held no registration secret, only its boot token, which an operator's
  daemon refuses (409), and the launch's Envoy and Dispatch bearers.
- **`… the daemon answered 403 Forbidden: Invalid operator token — the operator token does not match
  the daemon's operator_token_file`.** Your `operator_token_file` holds a different value than the
  daemon's.
- **`could not reach the Legion daemon at <daemon_url>: …; is the port-forward running?`** Fix
  `daemon_url`, pass `--daemon-url`, or start the tunnel; the command never tries another address.
- **`… is readable by its group or others (mode 0640); chmod 0600 it`.** The operator token and the
  NATS seed must be readable by you alone.
- **`stdin is not a terminal: Oh My Pi would run headless print mode, …`** Run
  `legion controller start` at a terminal. With no terminal, Oh My Pi would run the controller's
  start command headless, after the daemon had already revoked the running controller, so the
  command refuses before it asks the daemon for anything.
- **`unknown key "<key>" in the controller configuration; …`** The controller reads its own small
  file, not `legion.yaml`; compare with `deploy/kubernetes/daemon/controller.yaml.example`.
- **The plugin refusal.** `legion controller start` probes your Oh My Pi before it asks the daemon
  for a controller credential, and refuses one that does not load Legion's plugin or loads one speaking another
  daemon contract. Install the plugin from the same image as the daemon.

Whether the daemon sees a controller:

```sh
legion state --config legion.yaml --json | jq .controllerLocator
```

## An issue does not start

The daemon answers this itself for every issue that is not working now, in `why` on the issue:

```sh
legion state --config legion.yaml --json | jq '.issues["<KEY>"] | {why, waitingLine}'
```

`why.state` is `claim_stopped` for a claim nothing will wake (the stranded claims below),
`waiting` for an issue of the admission line, `blocked` for one a Dispatch link or an open
child below it holds, and `unlisted` for one the record holds in the workflow that stands in
none of the three admission lists; `why.reason` is the sentence. An issue holding an admission
slot carries no `why` unless it also stands in `admission.waiting` or `admission.blocked`, or a
stranded claim names it: an answered root reads `waiting`, a root whose open children hold its
own first phase reads `blocked`, and an issue at work whose worker runs, one `held` and a root
whose ask a person has not answered yet, which `waitingOn` answers for, read nothing. Neither
does an issue the record does not hold in the workflow (`done`, `backlog`, `icebox`,
`triage`). The daemon serves
`why` to a caller at contract 85 and above, which is where `waitingLine` gains `partPlace` and
the rest of the line's own members; from an older daemon, or an older pod, read the list that
follows.

Check, in order:

1. **The label.** The issue must carry the Dispatch label `legion`. A root without it is never
   admitted, whatever its status.
2. **The status.** A root runs from `todo`. In `triage`, a person's `legion` label is queued by the
   daemon, and the root moves to `todo` itself; a label a session set waits for the controller. A
   root the daemon recorded that a person moved back to `triage` is never queued again: it runs
   again only from `todo`. In `backlog` or `icebox` it does not run.
3. **The project and its repository.** The issue's project must be the daemon's, under `projects`
   in `legion.yaml`, and its repository one the daemon serves. An issue whose repository resolves to
   none, or to one not mapped to the project, or left out for a missing App, is not admitted: the
   daemon says why on the issue (`Legion did not admit this issue: …`), once until the cause
   changes. Set the issue's repository (the issue header's Repository picker, or
   `dispatch issue-update --repo owner/name`), or install the missing App, and move the issue to
   `todo` again. A child of a tree is told the same way, and its tree's architect gets a
   `child-not-recorded` notice.
4. **The slots.** When every slot is taken the issue waits its turn:

   ```sh
   legion state --config legion.yaml --json | jq .admission
   ```

   `active` lists the issues holding a slot, roots and children alike, `waiting` the issues in
   line, `trees` the roots of the trees that run, and `cap` the slots (`admission_cap`, which
   counts the issues the daemon has started, not trees). A parent whose own first phase waits for its open children
   is in `blocked`, with each child it waits for.

   Each waiting issue's own `waitingLine` says where it stands and why: `part`, `returned` for
   an issue whose work came back or `starting` for one that has not started; `kind`;
   `partPlace` and `partLength` within that part; `holdsSlot`, true where it holds its
   admission slot already and so waits for no free one; `since`, or the `rootRank`, `ownRank`
   and `rootKey` that placed it; and `reason`, the daemon's own sentence. Plain `legion state`
   prints that sentence for the first ten waiting issues under the `admission:` counts, each
   line reading `<KEY> <place> of <N>, <part>: <reason>`, and then `and <N> more`. It prints
   `waiting line: none` while nothing waits, and
   `waiting line: the daemon sent no reasons at this caller's contract` where the daemon served
   this caller no sentence. An issue that has not started stands behind every issue whose work
   came back, whatever its board rank, so that order is no fault to chase.

5. **GitHub refuses its branch.** Before the architect starts, the daemon creates the issue's
   branch, `legion/<KEY>`, on GitHub in the issue's recorded repository at its recorded default
   branch, as the implement App, and again before each tree member's planner starts, so no agent's
   push is the one that creates it (GitHub can refuse that push on a large repository). A branch
   GitHub already has is kept as it is. While GitHub refuses the read of the base branch's ref or
   the create, the architect or planner waiting for it does not start. The daemon retries, backing
   off to about a minute, and logs each refusal as an error whose `msg` is `outbox row failed`, with
   `kind` `issue_branch` and an `error` naming the repository, the ref, GitHub's status and its
   body:

   ```text
   {"level":"ERROR","msg":"outbox row failed","row":81,"kind":"issue_branch","error":"create the branch of WIDGETS-12, which its roles' starts wait for: create refs/heads/legion/WIDGETS-12 on acme/widgets at <default branch's commit>: GitHub answered POST /git/refs with 403: {\"message\":\"Resource not accessible by integration\",…}"}
   ```

   Fix what GitHub names, such as the implement App's permissions or a ruleset on `legion/*`
   branches. The next attempt then creates the branch, and the tree starts.
6. **Someone else holds it.** The architect's first act is to claim the issue in Dispatch. When a
   person or another session already holds the claim, the architect starts nothing and asks the
   holder, on the issue, to release it or take the issue back; the tree keeps its slot meanwhile.

## A tree waits on a person

When a root's architect asked a person a question on the root and nothing else of its tree runs,
its phase is `awaiting_person`: it keeps the slot it took when the daemon started it, while its
architect stays live to read the person's comments and replies. Read the wait:

```sh
legion state --config legion.yaml --json |
  jq '.issues["<KEY>"] | {phase, waitingOn, designGate, gatedSteps}'
```

`waitingOn.kind` is `ask` for a wait on a question, with `waitingOn.ask` the question, or `gate`
for a wait on the spec's approval, with `gateVersion` and `artifactId`; `since` is Dispatch's time
of the hand-back the wait began at. `answeredAt` means the wait was answered, and `answeredBy`
names how — not always by a person: `ask` or `reply` (a person's act on an ask of the root),
`changes_requested` (a person's changes request on the spec), `version` (a newer spec version once
the approval request is on the architect's turn), `closed` (a session or anyone else who is not a
person closed the question), `turn` (a session handed it back to the agent's turn), `retracted`
(Dispatch's settlement), and `approved` also when the spec's gate stands open with nobody's
approval on a deployment that gates no step, or when the last gated step was taken out of
`gates.design` while the root waited on it. The root is in `admission.active` throughout, since an
issue keeps its admission slot from the moment the daemon starts it
until it is done or leaves the workflow;
once it is answered it stands in `admission.waiting` too,
among the issues whose work came back, oldest first, until the pass that resumes it. That resume
takes no further slot, so it happens however full the cap is, whether or not the design gate has
opened. While the root stands in the line, `waitingLine` on the
issue gives its place in the line and the line's length, its `part` `returned`, its `kind`
`answered`, its `holdsSlot` true and a `reason` saying a person answered it and that it holds
its slot already, and its `why.state` is `waiting`. Nothing is sent to its architect about
the wait, and the daemon resumes the root by itself on that pass, so neither the
place nor the wait is anything to act on.
Do not park it or
start it by hand. Without `answeredAt`, check, in order:

1. **The question is still with the person.** `dispatch read <KEY>` shows the root's asks. A
   person's answer, resolution or reply on any ask of the root wakes it, whichever ask the wait
   names, and so does their approval or changes request of the spec, or the spec's gate standing
   open (`designGate.approvedVersion` equal to `currentVersion`) on the next admission pass. A
   question Dispatch retracted or a session closed, or one a session handed back to the agent's
   turn, wakes it once nothing else is with the person. An agent's answer or resolution of another
   ask never wakes it, nor does a person's act from before the wait began.
2. **The hand-over label.** A root whose `legion` label was taken off is not resumed: the daemon
   takes it out of the workflow (`phase: done`) and tells the controller `unslotted`. Put the label
   back and set the root to `todo` to queue it again.
3. **The Dispatch claim.** `dispatch read <KEY>` shows `Claimed by:`. A claim held by a person or a
   session outside the tree stops the resume and takes the root out of the workflow the same way,
   with an `unslotted` notice: Legion never resumes work another owner took. Release the claim,
   then `todo` with the label queues the root again.
4. **An answer the daemon never saw.** When `dispatch read <KEY>` shows the person already
   answered after `since` and none of the above holds, the answer's event never reached the
   daemon, and nothing re-reads a waiting root's asks until the next boot. Restarting the daemon
   (`legion restart`) catches the root up from its catch-up cursor: the boot applies the root's
   Dispatch event log after the cursor, with the root held until the whole window is in, then
   decides it once. A restart does not read an answer recorded before the root's first catch-up
   on this release, for a root recorded before the catch-up cursor existed and given no work
   since: that first catch-up only sets the cursor to the issue's latest sequence and replays
   nothing, and a boot whose read of the root Dispatch refused leaves it with no cursor, so the
   next boot's catch-up is that first one. In that case, or without a restart, have the person (or
   anyone acting as them) add a reply in the thread of any ask of the root, one that leaves the
   turn with the agent: that new act wakes the root on its own event.

A root whose architect registered its spec while the approval request waits on the person is in
`awaiting_person` with `waitingOn.kind` `gate`, once nothing else of its tree is at work; one still
`admitted` waiting on its design gate keeps its slot for one of the reasons above, or because
Dispatch shows the request on the architect's turn (a reply or a newer version since the request),
when the architect has nothing handed to the person. A root waiting on its gate at a daemon's deploy
is recorded as waiting at that daemon's first boot, keeping its slot. The gate stays closed until a person approves the
spec at its current version; `designGate.approvedVersion` is `null`, or lower than `currentVersion`,
until someone approves the latest version from the document's header or the Inbox. An approval of an
earlier version does not count: the spec changed since. The gate holds the tree's start, not its merge.
Where `gates.design` lists more than one step, `gatedSteps` carries one entry per step, each with
its own `artifactId`, `currentVersion`, `approvedVersion` and `status` (`unregistered`, `waiting`,
`approved` or `changes_requested`): the tree waits until every entry reads `approved`, and
`designGate` beside it is the `spec` step's record alone.

## A parked tree did not come back after a decision

A root in `backlog` or `icebox` runs again when a person sets it to `todo` with its `legion` label
on, when a person asks the controller to start it, when a person approves its spec and it keeps its
`legion` label and has no parent (the daemon then sets it to `todo` itself; an approval Dispatch
carries from an earlier one does not count), or, when the controller parked it for a hold of its
own, once the controller judges that a person's decision gives its work a next step or its walk
finds the hold's cause gone or its event happened. The controller marks such a park with a comment
on the root that begins `Parked by Legion while it waits on:`. First find whether the daemon relayed
the decision. Its JSON log names the issue and the root on each of these lines:

- `outbox parked decision sent to the controller` or `workflow: a person's decision on a parked
  tree goes to the controller`: the controller received the decision.
- `outbox parked decision not relayed: it names no person`: the gate opened on a deployment that
  gates no step, so no person made the decision.
- `outbox parked decision not relayed: its root is not parked` or `workflow: a decision on a
  lingering root someone moved back into the workflow is not relayed`: someone moved the
  lingering root back to a status of the workflow (`in_progress` and the like), so it is nobody's
  parked tree.
- `outbox parked decision not relayed: its tree hears it` or `outbox parked decision not relayed:
  its root has no record`: the tree runs and its architect hears the decision, or the daemon keeps
  no record of the root.
- `outbox parked decision handled before under this key`: the same notice came through again (a
  re-held copy, or a run of its row again); the first line logged under that key says what became
  of it.
- `outbox parked decision kept for its root's start, which waits for its slot` or `workflow: a
  person's decision on a tree waiting for its slot is kept for its architect's start`: the root was
  set to `todo` already, so no controller is told, and its architect's start names the decision.
  The daemon keeps the decision under the issue it was made on, so the architect it is owed to is
  the one of the tree that issue stands in now: a child handed over into a tree of its own, an
  orphaned child and a queued root adopted into a live tree each take what was decided on them
  with them. It is kept until the one start or catch-up that names it has told an architect of
  that tree — a catch-up at its publish, a start's task at the moment the turn that takes it
  starts — and one that reaches no architect hands it to a fresh catch-up of that tree's current
  architect (`outbox start finished without giving its claim the task; the decisions it named are
  handed over`), or leaves it for the next start while the tree is parked.
- `supervise: the root of this task's tree has no architect to hear it; the task stays pending`: a
  root architect's start task met a parked, lingering or waiting tree. It is sent when the tree's
  next start runs; nothing to do.
- `supervise: a decision this held task named is owed to another tree now; the task is sent without
  it`, or `supervise: everything this held task named is owed to another tree now; it is retired
  rather than sent`: while the task waited, an issue it named left this tree. What left is owed to
  the tree it stands in now, whose architect is written a catch-up naming it at once; nothing to do.
- `workflow: a session's reply on a parked tree is not relayed`, `workflow: a session's answer or
  resolution on a parked tree is not relayed`, or `workflow: a session's decision on a tree waiting
  for its slot is not kept`: an agent wrote it. It stays on the ask for the person who asked.

Then read the controller's comment that begins `Parked decision read:`, on the root and on the issue
where the decision was made. For a question, it is a reply on the question's own ask, which answers
it or says that `todo` on the root brings the architect back to answer. It says what the controller
judged. The tree stays parked when the park is not the controller's own hold, the park with its
`Parked by Legion while it waits on:` comment beside it: a person's park, another agent's session's,
or an operator's `legion status` (the comment names who, and the tree runs when a person sets it to
`todo`, approves its spec when it has no parent, or asks the controller to start it). It stays
parked too when the decision is older than the park, when the root is `done` or `triage`, or when
the decision only questions the work. When the controller was down, its next start walks the parked
roots. That walk reads only the last 10 events of each issue. The walk runs at each start and on
each `tick`, and its `todo` on the controller's own hold comes with a comment that cites the mark.

## An issue is held

**A provider's failure almost never holds the issue.** A turn that ends in an error the model's
provider returned keeps its task: the daemon sends the same task to the same session again after 30
seconds, then 60, then 120, doubling to at most 10 minutes between sends, with no attempt limit and
no new session. Each such turn logs the warning `supervise: the turn this task's prompt ran ended
in a provider error; the task is kept and sent again`, which names the model, the HTTP status and
whether the provider marked the error retryable, and the send that follows it logs `supervise: this
provider error can clear; the task goes to the same session again after a wait`, with the attempt
and the wait. The re-sent prompt tells the agent the turn stopped, names the HTTP status the
provider answered when it answered one, and carries none of the provider's own words. A content
filter is sent again because the session has already stepped down to the next model of its
fallback chain, so the next prompt goes to another model
([Starting fresh when a session is lost or wedged](https://github.com/sjawhar/legion/blob/main/docs/kubernetes.md#starting-fresh-when-a-session-is-lost-or-wedged));
a 429, a 408, a 401, a 403, a 5xx and an error the provider answered no status for are sent again
too: a credential error is the fleet's rather than this claim's work, so it climbs the ladder like
any other and the controller's notice at six is what tells a person. A newer
task for that claim replaces the one the error took back and is sent at once, so it does not wait
out a ladder that reaches ten minutes a step; while that claim's session is being compacted it
waits for the compaction's end instead, as everything the claim is owed does. An update round's
question waits behind the owed task, as any other task does: the question needs a turn of the
worker's own, and that turn would fail the way the task's did, so the pod's round waits until a
turn of the owed task ends with no provider error.

**One provider error does hold the issue**, for its architect: one whose HTTP status is a client
error, 400 to 499, that is none of 401, 403, 408 and 429. A malformed request is
the case it exists for. The claim fails with `why` reading `a provider error that cannot clear
ended this claim's turn: <cause>`, where the cause names that status, and the hold is the
architect's to retry or escalate as any held phase is. The claim's own record carries that
failure under the cause word `provider_error`, with the same sentence as its `detail`.

**After six turns of one claim in a row have ended in a provider error**, the project's controller
is told once, as `provider-errors on <KEY>`, with the run, the model and the cause
(`supervise: a run of turns that ended in a provider error; the controller is told`). Nothing is
held and nothing is stopped by that count: the task is still kept and still sent again. The run
goes back to zero three ways: at a turn of that claim that ends with no provider error, at a
compaction of its session that ran (`legion claims compact`, or the controller's `compact`; one
Oh My Pi refused or skipped resets nothing, since the session is as it was), and when the claim's
recorded session is forgotten, by `legion claims retry --fresh-session`, the controller's
`fresh_session`, or `legion claims forget-session`. Only after one of those can another notice
go. The notice is the claim's own — the issue and the role that names it — so
two claims of one issue each get a notice of their own, and in a provider outage every claim that
reaches six is told.

When an agent's budget runs out (launches that failed, deaths with work outstanding, prompts it
never took a turn on) the daemon stops relaunching it and holds the issue. The daemon logs JSON
lines, and the one to look for is the error whose `msg` is `supervise: claim failed`, whose `why`
says which budget ran out (`launch failures ran out`, `deaths with work outstanding ran out`,
`prompt retirements ran out`):

```text
{"level":"ERROR","msg":"supervise: claim failed","why":"launch failures ran out","launchFailures":3,"deaths":0,…}
```

The log is no longer the only place that cause lives. The daemon keeps it on the claim itself, as
`failure`. Its `cause` is one word out of `launch_refused`, `died`, `never_registered`,
`stale_plugin`, `prompt_retires_exhausted`, `provider_error` and `unrecorded`, the last for a
claim that failed before this release. Beside it stand `detail`, the sentence the log line wrote,
`at`, the moment it failed, and `attempts`, the four counts. A claim carries the record from its
failure until it is given another run. The issue's architect reads it with the `legion` tool's
`read_record`, the project's controller with `read_state`, and an operator with
`legion claims list`, which prints the cause word after the claim's state word and `-` for a
claim the daemon holds no cause and sentence for, or with `legion claims list --json` for the
whole record.

**A launch the shared public leak check refused** has a sentence of its own on the claim, as
`leakCheckCause`. Under the Kubernetes runtime the daemon reads that check at the tag the worker
image pins, and pushes it to the pod's launcher, which installs it before any role of the pod
starts ([Worker image](https://github.com/sjawhar/legion/blob/main/docs/kubernetes.md#worker-image)).
A start whose check could not be installed is not sent, and it is charged as a launch failure, so
a claim that keeps meeting it fails with `launch failures ran out: <the refusal>` and the cause
word `launch_refused`, or `died` where the refused launches were the relaunch of a process that
died. The sentence is the reason:

```text
the shared public leak check could not be installed: <what happened>
```

Every launch try sets it: to that sentence when the check refused the try, and to nothing when
the try started or failed for another reason. It is not cleared while the claim waits between
tries. It shows in `legion state --json`, `read_state` and `read_record` for a workflow role's
claim, and in `legion claims list --json` and the last column of `legion claims list` for every
claim. The project's controller is on no claim view of the state, so its sentence shows in the
listing alone. What follows the colon says where to look:

- `the daemon runs with no GitHub App, so it cannot read theorem-labs/agent-infra`, or `the daemon
  could not mint its read-only token for theorem-labs/agent-infra: …`: the daemon's GitHub App
  configuration, and the implement App's installation on theorem-labs.
- `GitHub refused to narrow the daemon's implement App token to theorem-labs/agent-infra alone, …`:
  the implement App's installation must reach theorem-labs/agent-infra. The daemon asks for no
  wider token in its place.
- `GitHub answered the read of theorem-labs/agent-infra at <tag> with HTTP <status>…`, or `the
  tarball of theorem-labs/agent-infra at <tag> …`: check that the tag the worker image pins exists
  in that repository and holds its `leak-check` directory.
- `the daemon could not read theorem-labs/agent-infra at <tag>: …`, `the daemon's read of
  theorem-labs/agent-infra at <tag> did not finish within <bound>`, `the daemon could not ask
  GitHub for …` or `the daemon could not slice the check it read at <tag>: …`: the daemon's own
  connection to GitHub, or what GitHub sent. The daemon keeps no failed read, so the next launch
  reads again.
- `the start stopped waiting for the daemon's read of theorem-labs/agent-infra at <tag>: …`: the
  launch reached its own bound before the daemon's read ended. The read goes on, and the next
  launch takes what it read.
- `the pod asked for the tag "<tag>", which is not of the published shape leak-check-v<n>`: the
  worker image's `ARG LEAK_CHECK_TAG` holds another shape of tag.
- `the pod's launcher did not answer the install of <tag> within <bound>`: the launcher's install
  took longer than the push's bound. The install goes on, and the next launch is answered at once
  when it is in place. The pod's log then shows `launcher: installed the shared public leak check
  <tag> at /legion/scratch/leak-check, served by the daemon (sha256 …)`: in the `roles` container
  of an issue pod, behind the role's prefix, and in the `controller` container of the
  controller's pod.
- every other sentence the pod's launcher gives, such as `the pod's launcher could not unpack the
  check the daemon served: …`, `the served check did not answer: …`, `the staging directory could
  not be made: …` or `the staged install could not be moved to …`: the launcher could not unpack,
  verify or move the check into place on the pod's disk. The sentence on the claim is the whole
  record, because the launcher writes no log line for a refusal.

Once the cause is fixed, `legion claims retry --claim <token>` starts a failed claim again. The
daemon launches a claim that has not failed yet again on its own, after the backoff below.

Before the `supervise: claim failed` line come `supervise: launch failed` or `supervise: process
died` warnings, one per attempt, each but the last followed by `supervise: waiting before the
next launch`: the tries are spaced by
a backoff (1–2 s, then 2–4 s), so a fault that lasts a moment cannot spend the whole budget. Under
the Kubernetes runtime, a launch whose requests the API server answers overloaded (a 429, or a 5xx
such as `etcdserver: throttle: too many requests`) waits the server out and is charged nothing;
its `sandbox runtime: the API server answered a launch's request overloaded` warning comes first,
and the launch fails only when the server is still overloaded at the launch's own deadline. The
state shows it too:

```sh
legion state --config legion.yaml --json | jq '.issues["<KEY>"] | {phase, holdReason, heldFrom, architect, workers}'
```

`phase` is `held`, `heldFrom` names the phase the hold took the issue out of, and the failed
claim's `state` is `failed` with its `failure` beside it. A held phase worker is its architect's
to retry or escalate, and `holdReason: "escalated"` means it went to the controller. An
architect that failed (`architect.state` is `failed`) leaves its tree with nobody to act inside it,
and the controller is woken. Unless the root's `phase` is `done`, it restarts the tree when it
finds no cause or the cause is gone, and asks a person whether to park it when the cause stays.
What brings the architect back depends on where its tree stands:

- **Its tree runs and does not linger:** `legion state`'s `claims nothing will
  wake` line names the architect under `failed, retry`. Retry it, as
  [A tree does not move after a restart](#a-tree-does-not-move-after-a-restart) shows: the daemon
  relaunches it on the session it kept, with fresh budgets, and the tree carries on at the
  generation and phase it stands at.
- **Its root is parked or lingers, or the controller restarted the tree** (`backlog`, then `todo`
  in the same turn): leave the claim as it is. The tree's re-admission retries it at a new
  generation; a retry by hand would run it outside the admission cap, and it would be sent no
  catch-up. To restart the tree by hand, write the same pair:

  ```sh
  legion status <KEY> backlog --config legion.yaml --operator-token-file operator-token
  legion status <KEY> todo    --config legion.yaml --operator-token-file operator-token
  ```

`legion claims retry --claim <token>` relaunches a failed architect on its session with fresh
budgets, and retries a failed phase worker whose issue is held for that worker's own role by
applying the held phase's retry, as its architect's `retry_or_escalate` would. It reaches a claim an
operator stopped as well as a failed one, since `legion claims stop` retires the claim it stops: a
stopped phase worker is started again on the task of the phase its issue stands in now. Every other
state is refused and named: a queued claim is spawned rather than retried, a suspended claim is
resumed, and the daemon's own controller is its keeper's to retry, which the refusal says. A retry
that leaves the issue held, as a lingering tree's does, is refused naming the issue.
`legion claims retry --failed` asks the same of every failed claim the daemon holds, in turn,
prints each one it retried, and exits 1 naming each it refused: run it once the cause of a run of
failed launches is gone (a host moved onto tmux, a broken key replaced). It passes over a stopped
claim, which is retired rather than failed, so name that one with `--claim`.

A held issue has one more release, and it costs nothing. `legion status <KEY> todo` on the tree's
own root releases the hold: the issue returns to the phase `heldFrom` names, that phase's worker
starts with its task, and the generation, the review rounds, the pod and the issue's place in the
admission line stay as they were. A person's move of the root to `todo` in the Dispatch dashboard
and the controller's own `set_status` do the same, since all three write the one status the
daemon reads. It reaches a root alone: a held child set to `todo` comes back at a new generation
of its own, and a root whose tree lingers or stands at `phase: done` is re-admitted at a new
generation, which is what the restart above does.

`legion claims compact --claim <token>` is the first recovery for a claim whose session a
provider keeps refusing: the daemon has Oh My Pi write a text summary of that session and then
sends the claim whatever task it owed again, so the session, its workspace and everything the
worker knows of its issue stay, and only the transcript the provider is asked to read gets shorter.
It reaches a claim whose agent is ready or idle, a root architect included, and refuses a
suspended, failed or retired claim naming what starts it again: `--fresh-session` for a workflow
role, and the daemon's controller keeper for the project's own controller claim, which only
`--claim` can name and which `--fresh-session` refuses in every state, a keeper whose resume an
operator's own suspension holds.
It also refuses a claim a suspension is held over, a claim whose launch is in flight, a claim
whose agent has registered and is not ready yet, which `--fresh-session` reaches, a claim left
working under a relaunch the wake pacing parked, and a claim whose role runs on Claude Code,
which has no Oh My Pi session.
For a claim the daemon records as working it reads that claim's process at the runtime, at the
moment of the request, rather than the daemon's memory of an older report, which a daemon restart
forgets, and that read records what it finds as the sweep's report of the process would: where
it finds the process running and clears a death charge, it writes the claim as the daemon holds
it, as that report's write does, and it stops, suspends, forgets, takes back and starts nothing.
A process that read finds running is refused
`CLAIM_IN_A_TURN`, because a compaction would abort the turn that may be the one ending the run
of refusals. A claim whose session is being compacted takes that code too. A claim whose process
the runtime would not answer for, and a claim that records no process to read, are refused
`CLAIM_UNSETTLED` instead, each left exactly as the request found it, since nobody read whether
a turn of it runs. A claim stored working that no process stands behind is refused
`CLAIM_NOT_RUNNING`, naming `--fresh-session` as what starts it again, because this command has
no agent to ask; for the project's own controller claim, which `--fresh-session` refuses and
whose keeper does nothing while it is stored working, it names
`legion claims suspend --claim <token>` and then `legion claims resume --claim <token>`.
A compaction Oh My Pi refused or skipped — a session too small, one already compacted — is
printed with its reason, and whatever task the claim owed has gone again all the same.

`legion claims retry --claim <token> --fresh-session` is the recovery for a claim the compaction
left refused, and for a claim stored working that no process stands behind, which the compaction
refuses by name: it forgets the session that claim recorded and starts the claim
in a new session with its phase's task, so no `legion claims forget-session` comes before it. It
reaches a claim whose agent is registered, ready or idle, one that is suspended,
failed or retired, and one stored working that the same read of its process finds nothing behind,
which is the dead role a project's controller has no other way back from, with no suspension held
over it, so it is what starts a claim the compaction could not reach, and a new session loses
everything the old one held. It suspends the claim where the request arrives, a live agent and
the claim no process stands behind alike, and that last claim comes back with the task its dead
turn held: the suspension takes that task back, and the start sends it again. It
forgets that session before it starts the phase, because the start runs the moment the restart
commits and would otherwise resume the very session you are dropping. A retry the daemon then
refuses has forgotten the session all the same: the claim keeps its state and its task, the
daemon's log names the session it dropped, and the same command works once the refusal's cause
is gone. A forget or a start the daemon refuses after this command stopped the claim is answered
200 instead, with that refusal and the claim's state printed under the claim's line: a suspended
claim is started by running the same command again, and a claim the refusal left in another
state, such as `launching` with its launch tried again at the probe interval, starts by itself.
What a refusal does on its own, before any of this, is in the paragraphs above and in
[Starting fresh when a session is lost or wedged](https://github.com/sjawhar/legion/blob/main/docs/kubernetes.md#starting-fresh-when-a-session-is-lost-or-wedged).
When the workflow refused that phase's restart — a lingering tree, whose members stay where they
stood, is the usual cause — the command prints a second line under the claim's own, saying the
claim is back in a new session owing no task and why nothing restarted its phase, and the
controller's `fresh_session` answers that same sentence as `phaseNotRestarted` rather than an
empty answer: the claim is running with no work, so clear what the line names and run the
command again to hand the claim its phase's task. `phaseNotRestarted` carries that sentence or
the one about the forget or the start that was refused after the claim was stopped, and never
both.

When a failed architect's session was lost with its volume, clear the stopped claim's matching
record before retrying:

```sh
legion claims forget-session --config legion.yaml --operator-token-file operator-token --claim <token> --session <recorded session id>
```

The daemon does not read or change the session database, so the old session row remains. A root
that was parked should be re-admitted after this command, not retried by hand; its next workflow
start spawns a fresh session. To replace a session of a claim that is still live, suspend it first;
the command refuses every live claim and any `--session` that does not equal the recorded session.
The resumed claim starts fresh with its pending task.

To see why the launches failed, read the failed role's lines of the `roles` container's log in the
issue's pod. The launcher prefixes each role's lines with the role's name:

```sh
kubectl -n legion get pods -l legion.dev/issue=<KEY>
kubectl -n legion logs <pod> -c roles | grep '^<role>: '   # e.g. '^implementer: '
kubectl -n legion describe pod <pod>     # scheduling, image pulls, mounts
```

## A tree does not move after a restart

A restart's wakes go out at the pace the operator set (`worker_wakes_per_minute`, 8 a minute by
default, with `worker_wake_burst` 1), so a tree whose process has not come back yet may simply be
waiting its turn. One wake has its session read its whole context again, which the provider charges
as uncached input — a mean of 576,083 tokens, a median of 493,636 and a ninetieth percentile of
1,327,888 — and the pacing is what spreads that over the minutes after a restart. A wave sustains
about two thirds of the rate, about 5 a minute at the default, so a hundred claims take about
twenty minutes. Read what the pacing still holds before you act on anything below:

```sh
legion state --config legion.yaml --json | jq '.wakesWaiting'
```

`legion state` prints the same as a `wakes waiting:` line, and prints no such line when nothing
waits.
Each entry names the `claim`, its `issue` and `role`, the `kind` of wake (`launch`, a role
process about to start; `plugin-refresh`, the relaunch of a session whose plugin speaks an
earlier daemon contract, which the daemon replaces at that one wake and asks nothing of first;
`catch-up`, the notice a start writes to an architect that is already running)
and `dueAt`, the moment the pacing gave it, which is when it runs.
An empty array says the wave has
gone out; the last `dueAt` in it says when the last of the wave goes.
A claim on that list needs
nothing from you: its wake is reserved and it runs at the moment named.
Your own requests are what
drop it — a resume, a retry or a task you send that claim is run at once, taking the reserved wake
back, so nothing you ask for waits for a turn.

After a boot onto a new plugin contract, the wakes alone understate the wave: the stale-plugin
lane holds at most one minute of the pacing at a time, so what it has reserved is a minute's
worth and what it has left is `.claimsToRefresh` of the same body.

```sh
legion state --config legion.yaml --json | jq '.claimsToRefresh'
```

That is how many sessions are still on an earlier plugin and waiting for the lane to relaunch
them, one paced wake each.
It falls to 0 as the wave drains.
A number that stands still over several minutes while no `plugin-refresh` wake waits is most often
a stale claim whose relaunch waits for a turn to end: the lane cuts no turn, so such a claim keeps
its place in the count and takes its one wake at the turn's end.
Read the claim the count still names (`legion state --json | jq '.issues'`): a claim in `working`
is that wait, and nothing is owed of you.
Only a count that stands still while every claim it names is idle is the reading worth acting on:
the daemon can relaunch no session onto a plugin at all, and its log says so once per claim at boot
(`this daemon knows no plugin versions to relaunch it onto`), which under Kubernetes means
`runtime.kubernetes.plugins` names no plugin pair.

That reading has a second cause, and the daemon keeps no record of it.
The lane's relaunch of a claim is refused for as long as the launch path refuses it — a tree
lifecycle cleanup already reserved for the claim's tree, an epoch the claim has moved past, or a
store that did not answer — and the sweep raises the relaunch again at each tick that has a credit
of the refresh gate free for it, until the refusal clears.
The claim keeps its place in the count through all of it, and it is handed no task either: a claim
whose `legion` calls this daemon refuses is given its task only once the process its relaunch
starts registers on this daemon's contract and reports ready, so the issue's phase waits for the
whole of the refusal.

Nothing the daemon keeps says that of one claim: the state carries no per-claim record of a
refresh that is not finishing, and the `update-stalled` notice the project's controller gets is of
a pod update round, never of a refresh.
What is left is the count and three log lines.
`claimsToRefresh` is the count for the class: it stands at what the lane has left to do, is 0 when
it has nothing left, and names no claim.
The lines name a claim, and each says less than the whole:

- **`supervise: the process speaks another plugin contract; relaunching it onto the current
  plugin`** at Warn, with `claim=`, is written once for each relaunch the lane starts, so a claim
  whose relaunch is refused has no such line at all.

- **`api: refused a call from a process on another plugin contract`** at Warn, with `claim=` and
  `route=`, is what the stale process's own `legion` calls are answered with, so a process that is
  idle writes none, and the line says the claim is stale rather than that it is stuck.

- **`supervise: an event failed`** at Error, with `claim=`, `event=supervise.StalePlugin` and the
  refusal under `error=`, is what each refused attempt leaves behind.
  It is the nearest thing to a per-claim signal: it is written once per attempt rather than once
  per claim, and only while something is refusing the relaunch.

So read the two together: a `claimsToRefresh` that does not fall while every claim it names is
idle, and, in the log, either the boot's one line per claim for the degraded daemon above or
`supervise: an event failed` naming the claim whose relaunch keeps being refused.

A restart re-adopts every process still running, but a claim with no process runs again only when
something starts it. A worker is started by the workflow when its phase begins, and a tree's
architect by its admission; a notice to an architect with no process waits for it rather than
starting it. A suspended worker of its issue's current phase that the daemon suspended is resumed
by the daemon at boot. One an operator suspended (`legion claims suspend`, one claim at a time or a
whole fleet before a restart), or whose suspension predates the release that records who suspended
it (`suspendedBy: unknown`), is not; neither is an operator-suspended or failed tree architect, nor
a claim whose launches ran out, nor a claim a close or a stop retired while its tree runs, whatever
its role. Each of those is woken by nothing: the daemon looks healthy and the
state shows the tree active, but the tree stops for good at the worker's phase, or at the first
decision that needs its architect. Once a boot has re-adopted its claims, the daemon names every
such claim in one error line, under the move that brings it back:

```text
{"level":"ERROR","msg":"legion daemon started with claims nothing will wake","project":"WIDGETS","count":4,"resume":["WIDGETS-12 architect"],"suspendedWorkers":["WIDGETS-12 implementer"],"retry":["WIDGETS-14 architect"],"needsWorkflowStart":["WIDGETS-15 reviewer"]}
```

The state names the same claims on every read, after a restart or not, each with its `state`, and
`legion state` prints them on its `claims nothing will wake` line:

```sh
legion state --config legion.yaml --json | jq '.strandedClaims'
```

Act only on the claims this list names, and leave out any you suspended on purpose. A claim's token
is `legion-<project>-<issue, lowercased>-<role>`, as `legion claims list` shows.

- **`resume`** (a suspended architect): resume it. Once it is ready the daemon sends it a catch-up
  of what changed while it was away:

  ```sh
  legion claims resume --config legion.yaml --operator-token-file operator-token --claim legion-widgets-widgets-12-architect
  ```

- **`suspendedWorkers`** (a suspended phase worker): the move depends on who suspended it, which
  `legion claims list --json` shows as `suspendedBy`, and `legion state` points there too.
  Only an architect is sent a catch-up; a worker resumed behind the entry its role's phases row
  names is sent that entry's work, which the daemon does on every admission pass and on a sweep
  each minute, and one already at that entry relaunches with nothing to do. Once it runs the list
  stops naming it.

  - **`suspendedBy: unknown`** (suspended before this release recorded who did): let the
    workflow start it. Stop the project's daemon, mark the claim as the daemon's, and start the
    daemon again; at boot it queues the workflow's own start for the worker's phase, with the
    right run, phase and, for a merger, approved head:

    ```sql
    update claims set suspended_by = 'daemon' where token = 'legion-widgets-widgets-12-implementer' and state = 'suspended';
    ```

    A task you deliver yourself belongs to no run of the workflow, so such a worker's completion
    can be refused (`HANDOFF_NO_RUN`, or a stale generation) when it never confirmed a task of the
    current run. The boot starts the worker only while its issue is in that phase: when
    `legion state` shows the issue `held`, leave the claim as it is, and its architect's retry of
    the phase starts it ([An issue is held](#an-issue-is-held)).
  - **`suspendedBy: operator`, with a task or a waiting start:** resume it. A worker suspended
    mid-task keeps that task (`legion claims list --json` shows it as the claim's `pending`, and
    the daemon refuses a second delivery), and its task is sent again once it is ready. A worker
    whose phase start reached it while it was suspended has that start waiting for your resume
    (the daemon logged `outbox row waits` for `start <role> of <issue>`, naming the operator's
    suspension); the start gives it the phase's task once it runs, so a delivery would hand it the
    phase twice.
  - **`suspendedBy: operator`, otherwise:** deliver its phase's resume task. An operator's
    delivery resumes the claim and hands it the task. The task is the one the workflow gives a
    worker resuming its phase, with the issue's title, key and phase as `legion state` shows them
    (a merger told no approved head sends the round back for review):

    ```sh
    legion claims deliver --config legion.yaml --operator-token-file operator-token --claim legion-widgets-widgets-12-implementer \
      --task "Continue <title>. Issue: WIDGETS-12. Phase: implementing. Resume the existing phase work."
    ```

- **`retry`** (a failed architect): retry it. The daemon relaunches it on the session it kept,
  with fresh budgets, and sends it a catch-up of its tree once it is ready, so the tree carries on
  at the generation and phase it stands at. The daemon drops every notice it could not deliver to
  the failed architect, the reviewer's approval that moved an issue into `accepting` among them,
  so the catch-up's reason names each issue of the tree waiting in `accepting` for the architect's
  acceptance; no worker runs there, and nothing else would wake the architect for it:

  ```sh
  legion claims retry --config legion.yaml --operator-token-file operator-token --claim legion-widgets-widgets-14-architect
  ```

  When a tree's architect and its workers failed together, retry the architect first: the held
  workers are its to retry once it runs.

- **`needsWorkflowStart`** (a failed phase worker, or a claim a close or a stop retired while its
  tree runs, whatever its role): a failed worker's issue is held
  ([An issue is held](#an-issue-is-held)), and its tree's architect retries the phase or escalates
  it to the controller; `legion claims retry` applies the same retry when the architect cannot, and
  a `todo` on a held root releases the hold without either of them. A
  failed claim ignores `legion claims resume` and `legion claims deliver`.
  A retired claim ignores those two as well, and `legion claims retry` refuses it because it has
  not failed, so what relaunches it on the session it kept is a start of the phase it owns — its
  architect's `retry_or_escalate` of a held phase, a transition into that phase, or a tree close
  and re-admission — and, for a retired tree architect, its tree's admission.

The list names only claims of trees that run — the trees `admission.trees` names, whether or not
an issue of them holds a slot — and leaves out claims that
something will still start: a worker whose phase is not its own is started again when its phase
comes, a parked tree's claims wait for its re-admission, and an issue whose start waits for a free
slot has that start queued already. A root in
`awaiting_person` is different: its architect stays live while it waits for the person's answer,
and the answer's task is delivered on the pass that reads the answer, which takes no slot for it. Never resume or retry by hand a
claim of a lingering tree or of an issue parked out of the workflow; re-admission or the issue's
return to `todo` starts it.
A claim an operator suspended is listed even when a workflow start is queued for it: that start
waits for the operator's resume. One whose `suspendedBy` is `unknown` is listed only while no start
is queued for it, since the next phase start or re-admission resumes it as before.

## A worker that stopped taking turns

The issue stands in its phase, its worker does nothing, and the daemon's record of it reads like a healthy one.
The claim is up and the state shows the tree at work.
The daemon finds such a worker by itself and writes one `worker-silent` notice to the issue's architect.
The project's controller reads the same issue in the `silent` list of its periodic tick.

The rule is this: the claim's process is up and between turns, it holds no task, and its agent's last recorded step is older than `worker_silence_minutes`.
That bound is 30 minutes by default, and the configuration refuses a value under 16 minutes when it loads.

The daemon sends the worker its work again, chosen by the phase entry the issue's phase row names for the role: the work of that entry where the worker's claim was never given it, and the phase's task again where the claim is on that entry already.
The notice says what the daemon wrote, which is that start row, so the worker is coming back and there is nothing to do.
A suspension an operator holds over the claim is the one thing that refuses the start when the outbox runs it, and the notice says so as well: resume the claim, or have the architect message the role and the controller start its session again with `fresh_session`.

The daemon writes the same record to its own log, as `workflow: a phase worker nothing will prompt was found silent; its architect is told`, so an operator finds every silence without a Dispatch notice.

The rule measures silence from the claim's last recorded step alone.
A daemon that was itself down for longer than the bound comes back and reads every live worker as silent.
Each of those gets one prompt and one notice, nothing is stopped, and the floor under `worker_silence_minutes` is what keeps an ordinary restart out of it.

A merged root's merger is never named silent.
The merge ends the merger's work, but the root stands in `merging` until the rest of its tree leaves the workflow, which can take days, so the daemon reads the issue's pull request and leaves alone a merger whose pull request the record holds as merged, as its own resume paths do.

A worker whose own session — the process the claim runs now — reports background work of its own running, an async job, a subagent or a background task, is left alone, however long it has been quiet.
No bound can tell a forty-minute job from a worker that stopped, so the daemon prompts no such worker at all.
When that report has stood past `worker_background_hours`, four hours by default, the daemon writes one `worker-background-stalled` notice to the issue's architect, naming the kinds the session reported and how long they have stood, and sends the worker nothing, because a background job can hang.
The architect reads what the session says it is waiting on and decides.
The report counts for the process the claim runs now and for no other.
A worker whose pod was replaced while a background task of its own ran left that report behind, and the work it names died with the pod, so the daemon reads such a worker as one that has reported nothing: it is prompted at the silence bound like any other, and it earns no `worker-background-stalled` notice.
That is why the daemon clears a worker's report when it launches the worker again, and why a worker on the Claude Code backend is repaired at all: that session reports at the end of a turn alone, and a worker nothing prompts has no turn to end.

## A commit carries another role's App as its author

Every role of an issue works in one jj workspace, and each role's commits must carry that role's own GitHub App as their author.
Before the daemon sends a role a phase task, it asks that role's shim to adopt the working copy under the role's App identity.
The shim runs `jj log` on `@`, which snapshots the working copy.
Then it runs `jj metaedit --update-author` on an `@` that has no description, or `jj new` on an `@` that has one.
So an adoption at the wrong time can make one role the author of another role's work.

**Which tasks adopt the working copy.**
The daemon adopts only for a task that names a phase, and only for the role that owns that phase:

- the phase start that a move into the phase sends to its role;

- the resume task of the phase owner.
  The daemon sends it in these cases:

  - the owner comes back ready with no task;

  - a boot resumes an owner that the daemon suspended;

  - a tree is re-admitted in the middle of a phase;

  - the daemon repairs a slot whose worker has no task, or a worker that went silent.

These tasks adopt nothing:

- the ask of a pod update round or a plugin update round;

- a wake for a CI settlement, a role message, an agent message or the backstop;

- an operator's task through `POST /legion/v1/operator/claims/<token>/deliver`;

- a look of a production check;

- the root architect's start, and an architect's catch-up;

- the controller's start.

Notices, hooks, phase-ended messages and phase reminders reach the agent through Envoy or through its plugin, not as a task, so they adopt nothing either.

**A phase task waits for the turns of the other roles.**
The daemon does not send a phase task while another claim of the same issue is in a turn, or while a prompt is on its way to one.
It sends the task when that turn ends, and the adoption runs then.

**Four lines in the daemon's log show what each adoption did.**
Each line carries the `claim` attribute, whose value is `legion-<project>-<key in lower case>-<role>`, for example `legion-widgets-widgets-12-implementer`.
So search the daemon's own log for the key and the `-` after it, without regard to case: the `-` keeps out the lines of a longer key, such as `WIDGETS-120` for `WIDGETS-12`.
Read the log of the daemon's pod under Kubernetes, or the output of `legion start` under tmux:

```sh
kubectl -n <namespace> logs <the daemon's pod> > daemon.log
grep -i '<KEY>-' daemon.log | grep -e 'adopted for the phase task' \
  -e 'rather than adopt the working copy' -e 'refused to adopt the working copy' \
  -e 'lost to the transport'
```

`kubectl logs` gives only the log file that the node keeps now, for the daemon's pod that runs now.
On a busy daemon, that file can hold only the last minutes, and a new daemon pod starts with an empty log.
Read the time of the first line.
If the wrong commit is older than that line, read the daemon's lines from the log store of your cluster.
A search that finds no line for a time before the first line does not show that no adoption occurred.

- `supervise: the working copy was adopted for the phase task` is one adoption that the shim answered `ok`.
  It names the `claim`, the `delivery`, the `phase`, the App's git name as `identity`, and the time the shim took as `took`, in nanoseconds.
  The release that carries cybermonklabs/legion#3971 is the first to write this line.

- `supervise: a claim of this issue is in a turn; the phase task waits rather than adopt the working copy they share` is a phase task that waits.
  `inTurn` names the claim whose turn holds it.

- `supervise: the shim refused to adopt the working copy; the prompt was never sent` is an adoption that failed, with the shim's own words.
  The task stays queued, and the refusal counts against the prompt budget of the claim.

- `supervise: the prompt was lost to the transport; the delivery waits` is a send that got no answer.
  When its `error` starts `adopt <claim>'s working copy for <identity>`, the daemon sent the adoption to the shim, and the answer did not come back: the connection closed, or the daemon stopped its wait.
  The shim runs jj on a time limit of its own, so jj can have run all the same, and the author can have moved.
  The task stays queued.

Compare the time of the wrong commit with these lines.
An adoption line for another role of the issue just before the commit shows that the adoption moved the author.
On a daemon that carries the rule, every adoption line names a phase, and its `claim` is the claim of the role that owns that phase.

A daemon from a release before `legion-v20.5.0` (cybermonklabs/legion#2539) adopts the working copy before every task that it sends, and it holds no task for the turn of another role.
On such a daemon, an update round or a message wake can take the author of the commit that the implementer works on.

**Other paths change an author, and no adoption line shows them.**

- A role's own jj command in the shared workspace snapshots the working copy under that role's identity.
  An architect's `jj git fetch` and the shell of a subagent are two such commands.
  The roles container gives every role one git identity, and a subagent's shell can carry another identity: `dispatch://INFRA-2791` holds both.

- `jj split` keeps the author of the commit that it splits, so a role that splits a commit that another role made keeps that other role's App on both halves.
  A check at push time that refuses a commit that the review App authored is `dispatch://INFRA-1185`.

## A pod does not come up

- **`Pending`.** Usually scheduling: a pod reserves its roles' cpu and memory (3 CPU and 19 GiB for
  an issue pod at the defaults) and lands wherever the pool has room, so a pool at its limits leaves
  the next pod unscheduled until the daemon reads it dead past `worker_boot_timeout_seconds` and,
  once its launch failures run out, fails the claim. `kubectl -n legion describe pod <pod>` names the
  reason. `admission_cap` counts the issues the daemon has started rather than the pods, and an
  issue that only waits keeps both its slot and its pod,
  so keep the pods a deployment's open trees run within what the pool's limits
  can place ([The cluster](/legion/legion/running-legion/#the-cluster)). A pod replaced on another
  node waits for no volume to detach: an issue's disk is its own pod's, created with it and
  deleted with it, so a replacement brings a disk of its own. The image probe pod,
  `legion-probe-<project>-<digest prefix>`,
  Pending Unschedulable at boot is the one pod the daemon waits on instead: it logs `boot probe is
  waiting on capacity; running it again` with the scheduler's reason and creates the pod again every
  5 minutes, serving nothing until one is placed and passes, and then reports the `pool-capacity`
  capability open with the count and the reason. Make room in the pool, or raise its limits; the
  probe pod itself needs only 250m and 1Gi.
- **The `workspace-init` container fails.** It provisions the issue's workspace on the pod's own
  disk, and its refusals name what to do, sometimes a `jj` command to run against the issue's
  clone (a local bookmark deleted and never pushed, or a conflicted one). Read it with

  ```sh
  kubectl -n legion logs <pod> -c workspace-init
  ```

  Nothing is registered before a refusal, so every later attempt in that pod refuses the same way.
  A pod whose `workspace-init` refuses runs no `roles` container, so there is no shell in which to
  run the commands it names; the remedy is letting the pod be replaced, and the issue's next launch
  provisions a fresh clone and workspace from its pushed branch on a disk of its own. Run no `jj`,
  `git` or `omp` in a running issue pod either: any role can plant configuration in its clone or
  workspace that starts a program with every role's credentials in reach (`docs/kubernetes.md` in
  the repository, "A shell on an issue's disk").
- **An issue's work is gone after its pod was replaced.** That is the shape of the runtime, not a
  failure: an issue's disk is its pod's own and is deleted with the pod, so a stop, a close, a
  node loss or a launch that replaces the pod takes the workspace with it. What survives is what
  was pushed to `legion/<issue>` and each role's conversation, which lives in the session
  database; the next pod provisions the workspace from that branch, and the agent is told its
  workspace is newer than its conversation before its next turn.
- **`worker-stream: rejected hello (stale worker generation)`** in the daemon's log is the fence
  working: a pod from an older generation of a claim tried to connect after a newer one replaced it.
  Nothing to do.
- **`worker-stream: could not resolve a hello's boot token; the shim redials`** means the daemon's
  Postgres did not answer while a pod's shim said hello. The hello is not refused: the shim
  redials, and the hello is accepted once the store answers. If the line keeps coming, look at the
  database, not the pod.
- **`supervise: the agent registered and never said it was ready; retired its process`** means an
  agent registered and its ready never came within the registration deadline
  (`worker_boot_timeout_seconds × worker_boot_registration_deadline_intervals`, 6 minutes at the
  defaults), often because the registration's answer never reached it. The daemon resumes the same
  session one generation later and counts a launch failure; after `launch_failure_limit` of them
  the claim fails.
  While the tree's cleanup is reserved (the tree is closing), the retirement waits: the process is
  kept, nothing is counted, and the deadline is tried again at the probe interval
  (`probe_interval_seconds`, 30 seconds at the default). Each try logs **`supervise: timer`** at
  Error with `timer=registration` and `error="retire <claim>, whose agent registered and never said
  it was ready: launch claim <claim>: bind work of tree <tree>: tree cleanup is reserved"` (`whose
  agent never registered` for an agent that never registered). A prompt retirement waits the same
  way, its error `retire <claim> after <n> prompt failures: launch claim <claim>: bind work of tree
  <tree>: tree cleanup is reserved`, on the Error line of the prompt failure that reached the limit:
  `supervise: timer` with `timer=turn` for a prompt that started no turn, `supervise: prompt
  outcome` for a prompt the agent refused, and `supervise: an event failed` with
  `event=supervise.StreamLateRefusal` for a late refusal. Nothing to do: the lines stop once the
  tree has closed.
- **`supervise: the tree's cleanup is reserved and the retired process is stopped; suspending the
  claim for the tree's close`** means the tree's cleanup was reserved while a retirement was
  stopping the process. The claim is suspended by the daemon with no process, and the tree's close
  retires it.
- **`supervise: the lifecycle check of the retired process's relaunch failed; trying it again at
  the probe interval`** means the daemon's Postgres did not answer the relaunch's lifecycle check
  after a retirement stopped the process. The claim waits `launching` with no process and keeps
  its task; the same relaunch is tried again every `probe_interval_seconds` until the store
  answers, and the agent is handed its task once it is ready. A daemon restart meanwhile launches
  the claim again as an unfinished launch. If the line keeps coming, look at the database.
- **`supervise: the launch could not be written; trying it again at the probe interval`** means
  the daemon's Postgres did not take a launch's write, after the launch had let the claim's process
  go. The process is stopped and nothing is started; the claim waits `launching` with no process,
  keeps its task, and the same launch is tried again every `probe_interval_seconds` until the write
  lands. If the line keeps coming, look at the database.
- **`supervise: a claim write the database did not take`** at Warn, with `store=` (the store
  method whose call failed) and `owed=`, means the daemon decided something about a claim and its
  Postgres did not take the write. With `owed=true` the write is kept and made again at that
  claim's next write that lands, which is the repair at the head of its next decision or a later
  write that decision makes of the claim as the daemon holds it: the daemon's memory is ahead of
  the row. The mark is held in memory only, so a daemon that stops while a write is owed loses
  that write and the reports it carried, as a stop between a decision and its write always has.
  The line comes again with `owed=true` at each decision whose repair fails again. With
  `owed=false` a repair's write reached the server, which judged the statement and would judge it
  the same way again, so the owed write and the reports it carried are dropped and the row stays
  as it was; that line also carries `droppedReports=`, how many reports went with it, and it is
  the only record that they were lost — that one is a defect to look at, not an outage. A
  decision's own write that fails drops the event that decision was handling, as it always has:
  one the server judged writes no such line and is the `supervise: an event failed` error with
  the store's message, and one it did not judge writes this line with `owed=true` beside that
  error. A repair that fails drops nothing, and the decision goes on.
- **`supervise: the store holds no such delivery; the refusal is of a retirement it has already
  made`** at Warn, with `delivery=`, means the daemon retired a task and the store answered that
  the claim's row holds no delivery at all: the retirement this daemon already made, whose answer
  an earlier broken connection lost. The line is written for that refusal and for no other. Memory
  drops the delivery to match the row and the claim goes on deciding, and the line is followed by
  the one above with `store=RetireDelivery` and `owed=true`, since that transaction rolled the
  claim write back with the delete and it is made again at the claim's next decision. The store's
  other refusal of the same delete — `the claim's row holds another delivery`, naming both the
  delivery asked for and the one the row holds — is not this one and writes no line of its own. It
  means a write that rotated the delivery's id, an interruption or a take-back, committed after
  its answer was cut off, so the store holds one delivery where the daemon's memory holds another.
  That refusal fails the decision as any other store failure does, reaching the log as `supervise:
  an event failed` with the store's message, and owes no write: read as a retirement already made
  it would drop the delivery memory holds and leave the stored one pending and retired by nobody.
  The decision that met it is the one event lost. The daemon marks the delivery its memory holds
  unwritten, so it writes that delivery over the row the rotation left, at the latest at the
  claim's next decision, and the retirement that follows it, once the task's turn is over, finds
  it under the id memory holds; the claim decides every event after the one the refusal cost, and
  no restart is needed to bring the row and memory together. A person's decision the rotated row
  held is not lost: the tree's next start or catch-up names it again, at worst a second time.
- **`supervise: a claim write the database did not take was made`** at Info, with `store=` (the
  store method that landed), is the close of that story: the owed write landed, and the row says
  what the daemon holds again. The reports that write carried — a turn that started, a claim that
  went ready or failed — are made right behind it, oldest first and before the report of the
  decision whose write landed, except one of a generation the claim has left (`supervise: an owed
  report is dropped; the claim has left the generation it was owed at`, at Info with its `state=`
  and `generation=`). A claim whose last `owed=true` line has no such line after it is still
  behind, and its next decision will try again; one whose last line is `owed=false` has dropped
  the write, and nothing tries it again.
- **`supervise: a turn started`** and **`supervise: a turn ended`** at Info, each with the claim
  and its `generation`, and with `delivery=` where the turn is a task's, are how a reader tells a
  turn that started from one the daemon never saw, and a turn that ended from one still running.
  A start with no end means the turn is running, or that it ended another way: look for
  `supervise: process died`, `supervise: the agent's turn has not ended; suspending it mid-turn`,
  `supervise: claim failed`, `supervise: the agent reported its exit`, `supervise: the process
  speaks another plugin contract; relaunching it onto the current plugin`, or `supervise: the
  update round is over for this claim`, each of which says what became of the turn instead.
- **`supervise: restoring a launch the previous daemon did not finish as launch_uncertain; no
  process is recorded for it`** at boot names a claim the store held live with no process, with the
  state it was stored in (`stored=`): a launch the previous daemon began and never recorded. The
  daemon relaunches its session once its orphan reconciliation has run. Nothing to do.
- **`supervise: refused a registration: the launch at its generation records no process`** and
  **`supervise: dropped a stale event`** with `event=StreamHello fence=process` mean a process
  registered or said hello for a launch the claim records no process for: a launch the daemon's
  stop cut short, or a retired process whose relaunch is waiting. The registration is refused with
  409 and the claim stays `launching`; the next boot, or the relaunch, starts the claim's process.
  Nothing to do.

## The work loops or stops

These arrive as messages on the Dispatch issue:

- **`Issue reached review_round_cap=3: 3 rounds counted, 0 conflict rounds not counted.`** The
  implementer's counted rounds reached three
  ([what counts a round](/legion/legion/concepts/#review-signalling)): every move back to an earlier
  phase raises the count, and a move made while the head conflicts with its base is a conflict
  round, which the cap leaves out and the message's second number names. The daemon posts it and
  tells the architect once for each value the cap holds, so the rounds after it say nothing again
  until you change `review_round_cap` or the issue runs again as a new generation. The work goes
  on, and the architect decides in the same turn whether to narrow the next rounds, split work off (a sibling child, or, on a root, an ask whether the rest needs an issue), or
  let rounds that are converging run, and posts that decision on the issue; it asks you only for a
  decision that needs your authority.
- **`Legion hook work-back fired at 5 moves back: testing to implementing. Prompted: implementer, tester, reviewer.`**
  A hook of your `legion.yaml` ([hooks](/legion/legion/running-legion/#legionyaml)) counted that
  move back and fired: the daemon wrote the hook's words to each role the line names, on that
  role's own topic, and posted this record of it. Nothing is held: the phase the move opened
  started its worker, and each role answers in its own turn. No hook writes to an architect: the
  daemon's own wake to an architect is a notice.
- **A red check.** Nothing moves for it. The daemon names every check GitHub reported, with the
  conclusion GitHub gave it, in one message each to the tree's architect, the worker of the
  issue's current phase and the implementer, and judges none of them. Re-run a check that failed
  on itself, have the implementer fix code that is at fault, or leave it to GitHub, which holds
  the merge under the base branch's own required checks.
- **A review round that no review decides.** Legion's reviewer must approve the head or request
  changes; a plain comment leaves the issue in `needs_review`, and the architect asks the reviewer
  for the decision. Only the review App or an account with write access to the repository decides a
  round, so an approval or request for changes from anyone else leaves it undecided too. A review
  from the review App decides a round only when it comes from the reviewer's own session: the
  daemon reads the review's Legion footer
  (`<!-- legion: {"session":"…","phase":"review","head":"…"} -->`) and requires the session it
  names to match the session recorded on the reviewer's claim. That footer also names the head the
  review read, and a review the review App submitted decides the round only when that head is the
  pull request's current head, compared exactly, whether the daemon heard of the review from its
  webhook or read it from GitHub's list. GitHub rewrites a review's `commit_id` to the current
  head, so its record of a review can name a head the review never saw, and no clock tells a force
  push that puts an earlier head back from the arrival of that head the first time; the footer is
  the review's own word for the code it read. A review-App review whose footer names another head,
  or none, is set aside and decides nothing, logged at warn as `workflow: a review decides nothing:
  footer names <sha>, not the head <sha>` or `… footer names no head`, with `footer_head` and
  `head`; the reason is kept on the reviewer's row and named in the stuck notice the architect
  gets, and the remedy is the reviewer submitting again at the head it read, which it takes from
  `gh pr view <n> --json headRefOid`. A person's review decides by its webhook on the `commit_id`
  that event carried; read from GitHub's list, where that id has moved, it decides nothing, logged
  as `workflow: a review decides nothing: a person's review read from the list`. A review-App
  review with no footer at all, or whose
  footer names another session (the controller's, an architect's), is set aside and decides
  nothing, logged at warn as `workflow: a review decides nothing: the review App submitted it from
  a session that is not the reviewer's`, with `footer_session`, `reviewer_session` and
  `body_truncated`. While the reviewer's session is not yet recorded, the login alone decides,
  logged as `workflow: the reviewer's session is not recorded; the review App's login decides`.
  Envoy caps review bodies at 2048 runes, so the daemon restores a truncated review-App body from
  GitHub before reading its footer; a review it cannot restore (no review id) stays set aside with
  `body_truncated=true`. The remedy for this stall: the reviewer re-submits its review with the
  footer naming its own session. The daemon reads the author's permission from GitHub before it
  applies the review: an account GitHub answers `404` for, or a `403` that is not its rate limit,
  has no write access, and stands so for five minutes; write access is read again for every review.
  Any other failed read is retried, a rate limit once the wait GitHub names has passed, and while
  that wait stands the daemon reads nothing from GitHub. The daemon logs each review that decides
  nothing for permission reasons as `workflow: a review decides nothing: its author is neither the
  review App nor an account with write access to the repository`, with the author's login, and logs
  a permission it read as no write access with GitHub's own answer. A `403` that is not a rate limit
  is logged at error as `workflow: GitHub refuses the review App's installation a review author's
  repository permission`, naming the installation's owner: the review App's installation cannot
  read the repository's collaborators, so until it can, no review but the review App's decides a
  round.
- **A review round stuck on an approval of an earlier head.** The architect is told `the reviewer
  approved <sha> on pull request #<n>, which does not approve head <sha>: a push since may have
  changed code, so only an APPROVE of <head> or a REQUEST_CHANGES ends the round`. The approval
  counts for the head it names and no other, since any push may change code, and a push on its way
  from that head leaves it undecided until the new head arrives. The remedy is the reviewer's
  approval of the head itself, which ends the round once the record reaches that head and GitHub
  reports that head one the issue may advance on.
- **The merger merges nothing.** It merges nothing while the head conflicts with its base, which
  GitHub can neither merge nor start pull-request CI for: the daemon sends such a head back to
  `implementing` on its own. And it enqueues nothing on a merge-queue base whose
  `squash_merge_commit_message` still joins the branch's commit messages, since the enqueue can
  carry no message of its own: it tells the architect, and only a person can change that
  repository setting to `PR_BODY` or `BLANK`. A merge GitHub refuses is the merger's to decide
  on, quoting GitHub's own answer: wait for what GitHub waits for, merge again at the head it
  re-reads, send the issue back, or tell the architect what only a person can change. A pull
  request a person already merged needs nothing: the daemon reads that merge and closes the
  issue — or, for a root whose tree still runs, once the last other issue of that tree is done or
  parked.
- **An implementer's completion refused `HANDOFF_NO_PULL_REQUEST`.** The implementer's completion
  of implementing reads GitHub, as the implement App, for the open pull request on the issue's
  branch, `legion/<KEY>`: the issue moves to testing only on an open pull request, or one the
  daemon recorded merged. One the daemon has not recorded yet (its opened webhook lost or still
  waiting) is recorded there and the issue moves on. A pull request merged while the issue was in
  `implementing` already told the architect (`pr-merged`), and the completion is applied and moves
  the issue to testing. With none, the completion is refused and nothing is recorded: `no open pull
  request on <owner>/<repo> legion/<KEY>; nothing was recorded. Open the pull request and complete
  again.` The implementer opens it and completes again at the same commit. When the record holds
  the issue's pull request closed without merging, the refusal adds `pull request #<n> is closed;
  your architect was told and decides this issue's phase.`: the close already told the architect,
  and a pull request closed without merging never moves an issue to testing. When
  GitHub cannot be read (the token mint or the read fails), the completion is applied unread and
  the daemon logs a warning, `api: read the open pull request on a completing implementer's
  branch; …`; the issue moves on once a pull request is recorded open, and when the daemon's next read of
  the issue finds none open on the branch the architect is told
  ([An architect is told `pull-request-missing`](#an-architect-is-told-pull-request-missing)).
- **A worker's completion refused `HANDOFF_STALE_ENTRY`.** The phase the completing turn was
  working ended under that turn, and the role has since been handed the phase's work again: the
  issue left the phase and came back, or the head started conflicting with its base while the
  tester tested, while the reviewer reviewed a round it had not completed, or while the merger
  worked toward the merge. The daemon ends no turn, so the turn reporting the completion is of
  an entry behind the one the role's phases row names, and nothing is recorded. The refusal
  names the issue, the role and the phase, the entry the completion is of and the entry the
  role's phases row is on, and then what decided it, which is one of four readings: the
  completion is at another commit than the head the record holds, or the record holds no pull
  request for the issue; it names no commit at all, so it says nothing about any head; the phase
  ends with no handoff record this role writes — retro and merging — so nothing the completion
  names says which entry's work it is, and no head it reports on answers for it; or the move
  that handed the phase out again moved the issue back, which asks for this phase's work to be
  done again whatever head the completion names. When the task of that entry already waits on
  the claim, the refusal says the turn began before it reached the session. It then tells the
  worker what to do in the same words the `legion` tool's description gives for the code
  (`packages/contracts/fixtures/daemon-api/stale-entry.json` holds the two equal): end the turn
  without completing; the task of the entry the phase is on is owed to the claim and the daemon
  sends it by itself, which it does on every admission pass and on a sweep each minute; and that
  task is work of its own, done at the head it names and completed with a handoff of its own,
  never the refused completion's record, commit or verdict. A testing entry after such a move is
  for the head the fix pushed, and the verdict on the head before it answers for nothing there.
  A completion at the head the record holds is taken rather than refused, where the phase ends
  with a handoff record this role writes and no move back raised the entry it is behind: the
  phase was handed out again at that same head, so the work the new entry's task asks for is the
  work that turn did. The same code on a completion of an entry above the one the phases row
  names is one the record never produces, since the daemon raises the row in the transaction
  that composes the entry's task; that refusal tells the worker to end the turn and tell the
  architect. Either refusal comes only while the role works the phase the issue stands in. A
  completion of another entry than the role's phases row names, from a role that no longer
  works the issue's phase — a taken completion sent again once it has moved the issue on, or one
  naming a newer record — is answered `HANDOFF_NOT_CURRENT_PHASE` instead, in the words the
  workflow answers that code with, `the <role> does not run phase <phase> of <KEY>; this
  completion changed nothing`, and nothing is recorded: the task of that role's entry was
  queued for the phase the issue has left, so the daemon drops it unsent and owes the claim
  none.

An issue back in `in_progress` from `retro` or `merging` has a head that conflicts with its base:
GitHub computes no merge ref for such a head and runs no check on it at all, so the daemon sent
the issue back to the implementer with the base to merge forward and told the architect. From
`retro` the implementer kept its turn and was handed the implementing task once that turn ended;
from `merging` the merger kept its turn, its jobs, its process and its session. Neither turn was
interrupted: each role was told on its own Envoy role topic that its phase had ended and that the
workspace is the implementer's now. The implementer's retro completion after the move reports the
entry retro handed it while its phases row is on the implementing one. The issue is in
`implementing` by then, so the `legion` tool sends it as the implementer's completion of
implementing, with the newest `implement` record and the head that record names, which can be the
head the record holds; it is refused (`HANDOFF_STALE_ENTRY`) because retro back to `implementing`
is a move back, which raised the entry the phase stands on, and the refusal names that move. The
implementer merges the base forward in the implementing task's own turn and completes that work
with a handoff of its own, never retro's commit; the merger's phases row still names the entry
merging gave it, so a completion it sends for the phase it lost is refused
(`HANDOFF_NOT_CURRENT_PHASE`).
The work comes back through testing, review, acceptance and retro to a new merge. A red check
sends nothing back from anywhere.

The pull request's state as the daemon sees it:

```sh
legion state --config legion.yaml --json | jq '.issues["<KEY>"].pullRequest'
```

The daemon reads the checks GitHub reports and judges none of them, so no state there holds work
back and no state there moves it. Read on GitHub what the base branch requires and what the head's
checks say: that pair is what decides whether the pull request can merge, and the merger waits on
GitHub for it.

## A repository change was not applied

The issue says `Legion keeps <owner/name> as this issue's repository; the change to <other> in
Dispatch is not applied`. An issue's repository is fixed when Legion first records it: its branch,
workspace and pull request live there, so a later change of the issue's own repository, or its
parent's or the project's default it inherited, does not move it. Create a new issue on the other
repository for the work that belongs there. The daemon says it once for each repository the issue
is changed to.

## A status change did not stick

The daemon owns a running tree's status. A status an agent session writes on a running root is set
back, and the tree's architect is told who wrote it. A person's move in the Dispatch dashboard, the
controller's `set_status`, or `legion status` from an operator's shell, is what takes a tree out.

A person's move between the workflow's own statuses on a running issue does not stick either. The
daemon's status repair runs at each boot and then every `controller_wake_interval_seconds` (an
hour by default), and writes back the status of the phase the issue stands in. `backlog` or
`icebox` takes the tree out of the workflow, and the repair leaves it there.

When the daemon's own status writes to Dispatch are failing, `legion state` says so on its
`pending Dispatch status writes` line:

```text
pending Dispatch status writes: 2; the oldest, for WIDGETS-12, has failed 4 attempts, next at 2026-10-03T10:41:07Z: …
```

The writes are retried in order; the error at the end of the line is Dispatch's answer.

## An issue's stop or start waits behind its own launch

An issue's lifecycle rows — its starts, stops and suspensions, its branch, its workspace removal —
run one at a time, oldest due first, so a stop, suspension or later start of an issue waits while
the row it names runs. Most often that row is a start whose pod is still launching
([A pod does not come up](/legion/legion/troubleshooting/#a-pod-does-not-come-up)). Other issues
are not held, and the issue's own status writes, messages and notices run meanwhile.
`legion state` says so on its `held lifecycle rows` line, naming the row that has waited longest:

```text
held lifecycle rows: 1; the longest waiting, supervise row 802 of WIDGETS-12, has waited 2m30s behind row 801
```

`legion state --json` lists each under `heldOutboxRows`, with its `id`, `kind` and `issue`,
`waitedSeconds` counted from when the row came due, and `waitsOn`, the id of the running row. Once
such a wait passes two minutes the daemon logs a warning, `outbox row held behind its issue's
running lifecycle row`, with `row`, `kind`, `issue`, `waitsOn` and `waited`: search the log for
it, then look at what the row `waitsOn` names is doing.

## GitHub events wait

A pull request's opened, push, review or CI settlement, or a Dispatch status change, reaches the
daemon as a message on the `ENVOY_NOTIFICATIONS` stream and waits there until the daemon's durable
consumer for it gets to it. `legion state` shows how far behind each consumer is on
its `consumer` lines ([Running Legion](/legion/legion/running-legion/#observe)), and `--json`
under `consumers`:

```sh
legion state --config legion.yaml --json | jq '.consumers[] | {name, waiting, oldestAgeSeconds, lastHandledAt, stalled, unavailable}'
```

- **`waiting`** is how many messages the consumer has not handled: not yet delivered, or delivered
  and not yet acknowledged. It counts the events the daemon acknowledges without acting on too: a
  pull request on a branch that is not an issue's `legion/<KEY>`, a review or CI settlement of a
  pull request the daemon does not record, and a run of a workflow that is no configured deploy.
  Those take no database transaction, so a backlog of them drains quickly. `0 waiting` is a
  consumer that is caught up.
- **`oldest`** (`oldestAt`, `oldestAgeSeconds`) is how long ago the oldest waiting message was
  stored: how long the event at the back of the line has waited. Seconds are normal. Minutes that
  keep growing are a consumer behind its stream, and once it handles a message stored more than
  five minutes before, the daemon logs `intake: a durable consumer is behind its stream; …`
  ([GitHub, NATS and the Secrets Broker](#github-nats-and-the-secrets-broker)).
- **`STALLED`** (`stalled`) is messages waiting and none of that consumer's handled for 60
  seconds, since the last one (`lastHandledAt`) or, before the first, since the daemon started.
  The daemon is handling none of them: each delivery fails and is put back to retry (the database
  out of reach, say), or the message in hand does not finish. Search the daemon's log for
  `retryable intake processing failure` and the other warnings naming a `subject` of that
  consumer, whose `error` says what fails. A handler that does not finish writes none of those
  lines while it holds ([a handling that outlives its window](#a-handling-that-outlives-its-window)).
  The stall is all that shows until the handler passes the intake handler limit and the daemon
  writes `an intake handler outran its limit and was cut`, so search for that line too
  ([GitHub, NATS and the Secrets Broker](#github-nats-and-the-secrets-broker)), whose entry
  says the limit this deployment runs with and where the daemon prints it. A consumer that
  stops is not a stall: it stops the daemon (`workflow intake stopped: …`).
- **`unavailable`** names what NATS did not answer in time, or refused; the rest of the state is
  served all the same. Every consumer is read at the same time, each within two seconds,
  so one consumer's read that does not answer leaves the others' as they are. `read the consumer's
  info: …` leaves `waiting` 0 and `stalled` false, since neither is known: check that the daemon
  reaches NATS and that the consumer exists (`nats consumer info ENVOY_NOTIFICATIONS <name>`).
  `read the oldest waiting message: …` leaves `waiting` and `stalled` standing and the oldest
  unknown (`oldest unknown` on the text line); that read has one second of its own. NATS refuses a
  request the daemon's user has no grant for without answering it, so a missing grant shows as
  `context deadline exceeded`, with the daemon's error line `NATS refused the daemon a permission:
  its NATS user lacks that grant` naming the subject. With subject
  `$JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS` it is the stored-message get
  ([Envoy](/legion/dispatch/envoy/#the-nats-grant-a-legion-daemon-needs)). A deployment gets that
  grant with the envoy-infra plugin release that carries it: until that release is applied, the
  oldest message is `unavailable` on each consumer while messages wait on it, and nothing else
  changes.

While a GitHub event waits, or was lost on its way, nothing comes round to make up for it on a
timer. The daemon's read of an issue's pull request
([Running Legion](/legion/legion/running-legion/#the-github-apps)) is the backstop, and it runs
at the daemon's start and after every handoff: it records the open pull request GitHub lists on
the issue's branch, and applies a head, a review or an approval-carrying push GitHub shows. An
event that waits for a phase no handoff is coming for waits for the consumer to catch up.

## An architect is told `pull-request-missing`

The notice's reason names the issue, the repository, the branch and the handoff commit: `<KEY>'s
implementer completed at <commit>, and GitHub shows no open pull request on <owner>/<repo>
legion/<KEY>, so <KEY> stays in implementing.` The daemon's read of that issue — made at its own start and after
every handoff — found an issue in `implementing` whose implementer recorded its handoff, with no
open or merged pull request recorded and none GitHub lists open on its branch. That happens
when the implementer's completion was applied without GitHub's answer (the read failed), or when
the pull request was closed without merging since. A pull request merged since tells the architect
`pr-merged` instead, and the implementer's completion moves the issue on it. The issue stays in
`implementing` until a pull request
is open on the branch: the implementer opens it, and the daemon records it from its webhook or
from its next read of the issue, and moves the issue to `testing`. When the work should not
go on, the architect decides the issue's phase as for a pull request closed without merging. The
notice comes once per handoff commit, from whichever read finds it first.

## GitHub, NATS and the Secrets Broker

- **`github_app_not_installed: <role> not installed on <owner>`.** Install that GitHub App (the
  implement or the review App) on the repository's owner.
- **`NATS refused the daemon a permission: its NATS user lacks that grant`** (an error line with
  the `operation` and `subject`). The daemon's NATS user is missing a grant;
  [Envoy](/legion/dispatch/envoy/) lists the subjects it needs. A daemon can boot healthy and
  consume events with one grant missing, so search the log for this line after a NATS change.
- **`intake: a durable consumer is behind its stream; every event it has not reached yet waits for
  it`** (a warning with `consumer`, `behindSeconds`, `pending` and `delivered`). The daemon is
  handling events JetStream stored more than five minutes before; it says so at most once a minute
  per consumer. `pending` near zero with `delivered` above one is most often one event retried
  after failures, not a backlog: a retried event keeps the time it was first stored. It can also
  be a second delivery of one event, which raises `delivered` with no failure of its own ([a
  handling that outlives its window](#a-handling-that-outlives-its-window)); the line `an intake
  fact was delivered again and dropped as already applied` is what names one.
  Every event that consumer has not reached waits: on the GitHub consumer a pull request's opened
  or push, on the Dispatch consumer a status change. `legion state`'s `consumer` lines
  (`consumers` in `--json`) show how many wait and how old the oldest is now, and whether the
  consumer has stalled ([GitHub events wait](#github-events-wait)). The daemon's read of an
  issue's pull request, at its start and after every handoff, still records the pull request
  GitHub lists, applies a lost head, review or approval-carrying push, and tells an architect
  `pull-request-missing`, so an issue whose next handoff lands does not wait for the backlog to
  drain.
- **`an intake handler outran its limit and was cut`** (a warning with `consumer`, `event_id`,
  `fact` and `limit`). One message's handling ran past `limit`, the handler limit
  `worker_rpc_timeout_seconds` derives for this deployment, so the daemon cancelled the
  handling's context and naked the message: nothing of that event was applied. Intake reports
  the message in progress only until the limit, and the handler returns once the step that is
  running honours the cancelled context, which every step does at once today; only then is
  this warning written and the message naked. The event is sent again, and its `delivered` count
  goes up by one, no sooner than that return: one nak delay (a second) after the nak, or at the
  consumer's next pull where the step held the handler past about one acknowledgement window
  after the limit ([a handling that outlives its window](#a-handling-that-outlives-its-window)).
  The reports stop at the limit, so a handler cut there has had no report for whatever it ran
  past the limit. A step that ignored the cancelled context would hold the message, and this
  warning, for the whole of its own run. Between this
  warning and the nak the daemon writes one more line under the same `event_id`, naming the
  step that was running, with the error `context deadline exceeded`; this warning is the one
  that names the cut. Every step is a `pgx` call or a Dispatch or GitHub read that gives up on
  a cancelled context, so the wait for the return is short, and each read is cut on its own at
  the acknowledgement window, so a handling that reaches the limit is most often waiting on the
  database. One Postgres advisory lock is what
  it most often waits on,
  `pg_advisory_xact_lock(hashtext('legion.apply_fact'))`, which every fact takes for the length of
  its transaction and the daemon's other lifecycle writes take too, so the holder is as often one
  of those as another fact. Ask the daemon's database who holds it rather than guessing:

  ```sql
  select a.pid, a.state, a.xact_start, a.query
    from pg_locks l
    join pg_stat_activity a on a.pid = l.pid
   where l.locktype = 'advisory'
     and l.classid = (hashtext('legion.apply_fact')::bigint >> 32 & 4294967295)::oid
     and l.objid = (hashtext('legion.apply_fact')::bigint & 4294967295)::oid
     and l.objsubid = 1
   order by l.granted desc, a.xact_start;
  ```

  The first row is the holder, the rest are waiting on it, and `xact_start` says how long it has
  held. The next delivery of the same event takes the same path and is cut the same way.

  The limit this deployment runs with is in the daemon's log at every boot, in a line beginning
  `intake: one fact's handling is cut after <n>s`, at warn when the limit is above an hour,
  where the cut comes too late to return a wedged handler's message while an operator is still
  looking. At the ten-minute floor that line names the floor as what decided the limit; above
  it, `legion start --check-config` prints the same sentence, which an operator can read before
  a rollout. The limit is one acknowledgement window for each of a review's three bounded reads
  and one more for the fact's transaction and its wait on the apply lock, never below ten
  minutes, so `worker_rpc_timeout_seconds` is the key that changes it.
- <a id="a-handling-that-outlives-its-window"></a>**`an intake fact outlived its acknowledgement window and was held in progress while it ran`**
  (an info line with `consumer`, `event_id`, `delivered` and `took`). The handling took longer
  than the consumer's acknowledgement window (`worker_rpc_timeout_seconds`), and intake reported
  the delivery in progress for `took`. The fact was applied: this is a fact that is slow, not one
  that failed, and this is the only line that says how long a fact took. One of them asks nothing
  of an operator; a `took` creeping towards the handler limit, or many at once, names where to
  look for the slow read behind them.

  The three entries above link here rather than say this again. Four things decide whether a
  second delivery of one event follows.

  - Nothing reaches this consumer while a handler runs. It asks for one message at a time and has
    no pull outstanding until that handler returns, except where the client reconnects and asks
    again.
  - The progress report keeps the message from falling due at all, while each report reaches the
    server before the window lapses: each report starts the window again.
  - A report that reaches the server late lets the message fall due while the handler still runs.
    An acknowledgement that is late, which goes out only after the handler returns, lets it fall
    due after that return.
  - An event that fell due is delivered again, and that delivery races the acknowledgement or nak
    the handler's return sends. Either side can win. Each measurement was made under one
    condition: with no progress report at all, the second delivery reached the consumer in 12 runs
    of 20; where a handler's step ran on after the cancelled context and held the message past the
    window, the second delivery won all 15 runs on an idle machine and lost to the nak only under
    load. Neither is the rate where the progress report reaches the server in time, nor where a
    cut handler stops on its cancelled context while its reports and its nak all reach the server
    inside the window: the event then never falls due, and the nak alone decides when it comes
    back. Where the nak is worked first, it takes the event out of the redelivery queue and resets
    its timer, so the event comes one nak delay later instead.

  What says a second delivery happened is a second line under the same `stream_seq`, `an intake
  fact was delivered again and dropped as already applied` with `delivered` one higher (the entry
  below). This line's own `delivered` is that delivery's count: the handling that outlived the
  window writes the line before its own acknowledgement.
- **`an intake fact was delivered again and dropped as already applied`** (an info line with
  `consumer`, `source`, `event_id`, `subject`, `stream_seq`, `delivery_seq` and `delivered`).
  Intake was handed a fact it had already applied, so it ran no handler, wrote no record and
  acknowledged the message. `delivered` and `subject` say where the second reading of the fact
  came from, and there are five. A `delivered` above 1 is a JetStream redelivery of the same
  message. A `delivered` of 1 is a second reading of one fact, in one of four ways: the
  listener publishes a review or comment that mentions the trigger on each of its mention
  topics beside its own, one fact under one dedupe key; a durable consumer created again at the
  last message it applied is delivered that message once more (the `intake: a missing durable
  consumer is created at the last message it applied` entry below); the listener publishes a
  GitHub delivery again, which the fact's dedupe key drops, where the stream's own duplicate
  window did not refuse it first, so either the redelivery was asked for more than 72 hours
  after the delivery it repeats or the envelope carried no message id; and where
  `repository_renames` has the daemon hear one repository under two names, the copy published
  under the other name reaches the same durable, with the same fact and a subject of its own.
  A `delivered` of 0 is no reading of a drop at all: the daemon's read of the message's
  metadata failed, so the line carries five fields, with an empty `consumer` and no sequences.
  The line asks nothing of an operator on its own. Two numbers are read from these lines. Their
  total is this consumer's share of what `ApplyFact`'s order costs: every copy dropped here
  took the fact lock before the deduplication found it applied, whichever reading it is, and
  `ApplyFact`'s other callers drop duplicates the same way and write no line, so the total is
  the intake consumer's part of what decides whether `processed_events` should be read before
  the lock. The lines whose `delivered` is above 1 are the rate at which JetStream delivers again
  a fact this consumer had already applied. The daemon writes a line only for a copy it finds
  already applied, so that rate counts no redelivery of an event that carries no fact, and none of
  an event not yet applied. A recreated consumer's replay reads 1 and stands outside the rate,
  although JetStream did hand the fact over twice.
- **`webhook App missing on <repo>: <reason>`** in `legion state` (`repositories.withoutWebhookApp`
  in `--json`). Dispatch's GitHub App, the Envoy App whose webhook carries GitHub's events, is not
  known to be installed on a repository the daemon serves. With `Dispatch's GitHub App is not
  installed on <repo>: …`, GitHub sends Legion none of that repository's events and nothing
  redelivers them: the repository is served, and nothing brings its pull requests' changes in, so
  its issues stand still. An operator installs Dispatch's GitHub App on the repository
  ([The GitHub Apps](/legion/legion/running-legion/#the-github-apps)); the line goes at the
  daemon's next read of the mapping, at a restart or the next change of the mapping in Dispatch.
  `the webhook App check failed: GITHUB_APP_UNCONFIGURED: …` is a Dispatch that holds no App key
  (`DISPATCH_APP_CLIENT_ID`, `DISPATCH_APP_PEM_B64`); `GITHUB_UNREADABLE` is Dispatch's read of
  GitHub failing; `this Dispatch does not answer the webhook App check` is a Dispatch older than
  the check: upgrade it. None of these leaves the repository out or stops the daemon.
- **`intake: a missing durable consumer is created at the last message it applied`** (an info line
  with `consumer` and `start`). One of the daemon's durable consumers was gone when the daemon
  opened it, at boot or when the project's repositories changed (deleted by hand, or its stream
  recreated), and the daemon created it again starting at the stream time of the last message it
  applied, so the messages published while it was missing are delivered; the one at `start`,
  applied already, is dropped as a duplicate. Nothing to do. A consumer the daemon has no applied
  message recorded for starts at the next message instead, with no line.
- **`intake: a missing durable consumer's last applied message is older than the stream's first
  message; …`** (a warning with `consumer`, `start` and `stream_first`). The stream keeps 72 hours
  of messages, and the last message the consumer applied is older than the oldest it still holds.
  Only an applied event moves that position, not one the daemon acknowledged without acting on, so
  on a quiet project the consumer may have handled every event between `start` and `stream_first`
  before it went missing. Any it had not reached may have been missed: the daemon applied none of
  them, and nothing replays them, so an issue one of them would have moved waits for its next
  event, or for the daemon's own read of its pull request at that issue's next handoff.
- **`agent-secrets: the token login failed; retrying`** (an error line with the cause, the class
  it was sorted into and the retry schedule that class takes).
  With `runtime.kubernetes.agent_secrets` set, the daemon logs in to the
  [Secrets Broker](/legion/broker/) with its own ServiceAccount token, read from `token_file`, at
  every start and again before each credential expires; pods wait to be enrolled until a login
  succeeds. The answer says why: `ACCOUNT_NOT_LISTED` (the broker's `BROKER_SERVICE_LOGINS` does not
  name the daemon's ServiceAccount for `legion-daemon`: add it and redeploy the broker),
  `TOKEN_EXPIRED` or `TOKEN_INVALID` (the token file is stale, or not a token for the broker's
  issuer and audience: check the daemon's projected-token volume), `TOKEN_TOO_LONG_LIVED` (the
  token lives longer than an hour from its issue: set the projected token's `expirationSeconds`,
  or `kubectl create token`'s `--duration`, to at most an hour), `TOKEN_LOGIN_OFF` (the broker
  trusts no Kubernetes token issuer), `read the ServiceAccount token` (the file is missing), a
  `404` from a broker too old to have the token route, or a network fault. `.agentSecretsLogin`
  reads `"state": "failed"` until a login succeeds. Nothing needs restarting, and no person is ever
  asked. The line names the class the failure was sorted into (`class`), the retry schedule that
  class takes (`schedule`), the wait before the next login (`retry_in`) and the cause (`error`).
  When a login got no HTTP answer, or a 5xx answer from the broker or its front door (the
  broker is down, out of reach, or cannot reach its database), the daemon retries 2 s later,
  doubling to at most 10 s, so it logs in within about 10 s of the broker serving again. After
  any other failure it retries 30 s later, doubling to at most 5 minutes: a refusal the broker
  decided, a token file it could not read, and a fault on the daemon's own side — a TLS
  handshake that failed, or a host name that does not resolve — which no wait of seconds fixes.

  ```sh
  legion state --config legion.yaml --json | jq .agentSecretsLogin
  ```
- **`runtime.kubernetes.agent_secrets.token_file is required`.** The daemon logs in with its own
  ServiceAccount token, so the block names where it is mounted.
- **`runtime.kubernetes.agent_secrets.operator was removed (LEGION-664)`.** No person approves the
  daemon's login, so the block names no approver. Delete the key.
- **`supervise: agent-secrets: the broker found the pod token expired; keeping the key for a fresh
  token`** (`403 POD_TOKEN_EXPIRED: the projected token has expired (oidc: token expired at
  <time>)`). The token the daemon presented for the pod expired before the broker checked it. The
  daemon reads a fresh token from the shim when the one it holds is about to expire, so from a
  current daemon this means the broker's clock runs more than a minute ahead of the daemon's. The
  daemon keeps the pod's key, asks the shim for the pod's current token at its next observation,
  and enrolls the key with it; check the two hosts' clocks.
- **`supervise: agent-secrets: enrollment refused; waiting for the shim's next hello`** with
  `403 POD_IDENTITY_MISMATCH: the projected token does not identify this pod`. The token is not
  this pod's: it names another pod UID, it is not signed by the cluster the broker trusts
  (`BROKER_K8S_OIDC_ISSUER`), or it is for another audience. An expired token is answered
  `POD_TOKEN_EXPIRED` instead.
