# The end-to-end scripts

Each script here runs real binaries built from the checkout against real dependencies on the machine it runs on and fails loudly on the first step that does not hold.
Each is a step of a job of `ci.yaml`, which gives it what it needs ([Where each script runs](#where-each-script-runs)), but for two tools, `step-records-read.sh` and `plan-ratings-read.sh`.
`lib/` holds what the scripts share:
standalone helpers they run, and the files they source, which `shellcheck` follows from this directory through `scripts/e2e/.shellcheckrc`:

- [`lib/rig.sh`](#librigsh)

- [`lib/process-tree.sh`](#libprocess-treesh)

- [`lib/kind-sandbox.sh`](#libkind-sandboxsh)

- [`lib/omp-home.sh`](#libomp-homesh)

This file uses semantic line breaks: one sentence per line, and a long sentence may break after a clause.
A blank line separates list items.
Add a new item next to the item it belongs with, never at the end of a list unless it belongs there, so two pull requests that add items or change different sentences do not conflict.
A new script's section, and its item in the index list below, go next to the script it is most like, never after the last script or before the first `lib/` section unless that is where it belongs.
`bun scripts/semantic-line-breaks.ts scripts/e2e/README.md` checks the layout, and `--write` fixes it.

Every `docker run` in these scripts sets a CPU and a memory limit inline, and its container is removed when the run ends (an `EXIT` trap's `docker rm -f -v`, or `--rm`):
a script shares its machine with the other steps of its job, and an unbounded container takes cores and memory from every other process there.
A container is named for the pid of the driver that started it, and the kernel hands a pid out again, so a teardown removes by name only what its own run started.
What an earlier run left stays for [`lib/leftovers.sh`](#libleftoverssh) to name to the operator.
Every scratch Postgres keeps its data directory on `tmpfs`, so no run leaves a volume behind, and tmpfs is charged to the container's memory.
Every scratch Postgres gets `--cpus 2 --memory 2g`.
NATS gets `--cpus 1` (0.5 in `lib/nats-stream.test.ts`) `--memory
512m`, enough for one run's JetStream streams.
The kind job's rig ([`lib/kind-sandbox.sh`](#libkind-sandboxsh)) runs on a runner of its own, and kind's node container, which `kind create cluster` starts, has no limit, because the kubelet inside it reads the host's capacity, not a cgroup cap:
a smaller cap on the node lets the kernel kill pod processes below their own limits, and the kubelet does not see why.

Every `legion.yaml` a proof writes for a Go daemon sets `worker_wakes_per_minute: 0`, which turns the daemon's wake pacing off (INFRA-976).
A proof starts roles and waits on bounded times, and at the default of 8 wakes a minute every wake after the first would spend those waits queueing for its turn.

Every stage proof marks each process it starts with `LEGION_E2E_RUN=$work` once its transcript runs, and its teardown and its checks that no process remains take only the processes that carry the mark ([`lib/process-tree.sh`](#libprocess-treesh)'s `marked_processes`):
a person's shell or `tail -f` working under the run's directory is never signalled or waited on.
A host that runs them needs GNU findutils 4.9 or later (`find -files0-from`, which Ubuntu 24.04 ships and 22.04's 4.8 does not): with an older `find` the port-holder read (`listening_holders`) and the leftovers check (`refuse_leftovers`) exit 2 at every call.

Each item below names a script, then what it proves.

- `stage1-skeleton.sh`

  `legion start` boots against a local Postgres, serves `/healthz` and `GET /legion/v1/state`, answers `GET /readyz` 200 once it serves, read after `/healthz` at the first boot and again after the restart, since the readiness latch is one-way and the booting process's own, so a daemon that answers `/healthz` while it never latches is what each of the two reads catches;
  answers `legion state` with the 21-row capability report on it (`resource-limits` open under tmux, carrying the `capabilities.decided` line to write;
  `pool-capacity` present, since tmux runs no probe pod), registers itself in the Go daemon's own legions registry, survives a restart against the same store with its first boot time intact, refuses a wrong Postgres password at once by the host and never by the password, and, as a second daemon behind a proxy that resets its first two connections to that Postgres, opens its store by trying again 1 s and then 2 s later, and serves

- `stage2-tmux-supervision.sh`

  the Go daemon supervises real Oh My Pi sessions
  — the build `LEGION_TEST_OMP` names, with this checkout's two plugins in an isolated profile
  — in its private tmux server, against a real Envoy listener and NATS and an Anthropic Messages stand-in:
  the plugin gate refuses another contract, a disabled plugin, a profile without the Envoy plugin, a missing skill, a skill only the role prompts load, and a task agent whose model role no one configured;
  an agent registers, holds its Envoy role and is ready;
  a task queued before ready runs once, its model turn through the configured route, and a retried frame starts no second turn;
  a killed pane resumes the same session; suspend and resume keep it;
  a stale hello is refused; an agent that never registers is retired at the deadline and counted; a restart re-adopts every live claim.
  The `agent-harness` job of `ci.yaml` runs it

- `leak-guard-image-probe.sh`

  the worker image's own `git` and `jj` refuse a push that names something private, run with `docker run` inside the image the `worker-image` job has just built and pushed:
  k1 of dispatch://INFRA-1527 — a commit whose message carries a refusable Tailscale MagicDNS host, one whose author identity carries it, one whose only hit is an added line of a tracked file, one whose only hit is the branch name, and a clean one — under `git push` and under `jj git push` in each shape jj's own help in that image enumerates (the bare shape, `--all`, `--change`, `--named` and `--deleted`, the last a ref change with no commit), with `git ls-remote` read before and after every push, the added-line case and the branch-name case reported as checks of their own, and the run failed outright where a shape the script drives is gone from jj;
  and k7's two shared-check failures at those same call sites, the stand-in check removed so that none resolves and a stand-in that exits 2 for an internal error, each of which must refuse under both commands and name the failure it met.
  A container started from the image runs no launcher, so the daemon pushes it no shared check.
  It writes its own stand-in shared check into its scratch directory, and the rule file embedded in the image's `legion` decides every verdict;
  k7's other two failures cannot be met in an image at all, since the rules are embedded in that binary and the engine is linked into it, which the run says in its own log.
  Its control is the same image with `/opt/legion/bin` taken off `PATH`, where the real `git` and `jj` let all five cases through.
  Each check prints one `CHECK <name>: PASS|FAIL` line.
  The `worker-image` job of `ci.yaml` runs it after its build

- `controller-start-tmux.sh`

  the operator-launched controller on the Go daemon under tmux:
  `legion config check` passes a real config, reporting after its OK line the two deployment capabilities a tmux file alone leaves open (secrets, resource-limits) and then that a daemon under tmux cannot pause its project (`pause unavailable: runtime: tmux`), and names the key on each broken variant, running no key command;
  the boot gate refuses a plugin of another contract;
  `legion state --config` runs no key command;
  `legion controller start` refuses a group-readable operator token file, claims the controller role, shows in `controllerLocator`, runs Oh My Pi interactive with the controller environment and its secret only as a file, leaves Ctrl-C to Oh My Pi, and exits with its code;
  `legion issue status` from an operator shell mints its grant with the operator bearer;
  a second start revokes the first's capability and grants; the controller liveness probe reads the live listener.
  The `agent-harness` job of `ci.yaml` runs it

- `dispatch-user-turns.sh`

  a person's direct Send or Aside from Dispatch's conversation page reaches a real Oh My Pi session
  — the pinned build with this checkout's Envoy plugin in an isolated profile
  — as that person's own user turn, the body alone, carrying its Dispatch id as the host's tag, while a BTW stays a side question;
  the composer completes the session's slash commands, and a skill, a prompt template, a file command, an extension command, `/session`, `/compact`, `/new` and an unknown `/foo` each run from the page as they would at the session's terminal;
  a frame a session forged claiming a person wrote it, a broadcast, an issue message, a Legion role notice and a session's re-send of the person's BTW through the retry route each arrive as a card;
  a Send the session got as a card stays one when a frame is forged for it inside the accept's minute, while the person's retry of it is their turn;
  the page shows each message once; after the session restarts, a replay of the Send's own envelope and a frame forged naming a Send made while it was down, over a minute old, each inject nothing, and neither does a frame a bare bus client forges for a failed Send inside its minute;
  and Dispatch records only the Send, the Aside and the carded Send's retry as accepted.
  The `session-scripts` part of the `agent-harness` job runs it

- `dispatch-session-settings.sh`

  Dispatch's live view shows a real Oh My Pi session's current model and thinking level and switches them (INFRA-195):
  Oh My Pi in `--mode rpc` with this checkout's Envoy plugin, a real listener, `nats-server`, Postgres and Dispatch, and one held-open Chromium page.
  Each value the header shows matches the session's own RPC answers, its `/effort` output and its session file:
  before any turn; within 20 s of a change at the terminal, by a message or by a fallback;
  after a pick from either picker, including `auto` kept across a model pick, the level a model pick leaves (compared with a twin session), a pick during a turn, a refusal, and a pick to a stopped session that ends in the no-answer line and is refused `ACCEPT_STALE` once it resumes.
  A forged switch, an Envoy message and an agent's bearer change nothing.
  Another session, a resumed session and a new session each keep their own values.
  A session on the released pi-envoy shows the restart notice.
  A switch is stored under the person who sent it.
  The `session-settings` part of the `agent-harness` job runs it

- `clean-session-stop-tmux.sh`

  the daemon's clean stop of a dead role's Envoy session, measured (INFRA-392), against a listener and a NATS built from this checkout and an Anthropic Messages stand-in both harnesses call, over a real `legion start` daemon under `runtime: tmux` with a private tmux server of its own:
  an architect on Oh My Pi and an implementer `worker_backends` puts on Claude Code each register, and the agent process of each — the `omp` under `legion worker-shim`, the `claude` under `legion claude-bridge` — `SIGKILL`ed in its pane, is read `gone` by `GET /v1/registry/<session>?include=unresponsive` within ten seconds, the latency measured and printed, with the daemon's own log line naming the session it stopped.
  The listener's gone horizon is 90 seconds, so the ten seconds measure the clean stop and not the horizon;
  that a session no daemon launched, killed the same way, waits out the horizon is `packages/envoy-client/test/session-stall.ts`'s.
  `CLEAN_SESSION_STOP_HEARTBEAT_MS` (default 5000) is how often every session of the run heartbeats its registration: the heartbeat decides how often a kill lands just after a registration, which is the window a clean stop dated before the moment its probe judged refused a stop that was owed in (dispatch://INFRA-2064), so a small value makes a kill inside that window likely (3 of 4 runs at main with 500) and the defect's reproduction a command rather than an edit.
  The `agent-harness` job of `ci.yaml` runs it

- `dispatch-claimant-delivery.sh`

  Dispatch's outbox delivers a person's answer, comment or untargeted message on an issue a session has claimed to that session's own `notifications.agent.<session>`, driven over HTTP through the real `envoy-dispatch` against a loopback PostgreSQL the caller names and a NATS of the run's own:
  a person's answer to an ask another session opened reaches the asker and the claimant, a person's reply in a person's thread on the issue's spec and a person's untargeted message reach the claimant, while a message targeted at another session, another session's comment and a person's comment on an unclaimed issue reach no claimant.
  A person's comment mentioning a session skips that session's own copy only when the mention's send landed:
  a delivered mention of the claimant leaves its topic out, a failed one reaches it, and a failed mention of an ask's follower still reaches the follower.
  Run against a base binary (`DISPATCH_BIN`), it is the change's negative control

- `verification-results.sh`

  a recorded check result says where its commit stands against the commits and branches its check compares with, and the head it is compared with moves with the pull-request events Dispatch takes off the bus, driven over HTTP through the real `envoy-dispatch` against a loopback PostgreSQL the caller names, a NATS of the run's own and the e2e harness's fake GitHub:
  a result is read back under its check;
  a push that moves the pull request's head, published as the normalized `pull_request` envelope, leaves the earlier result at none of the commits the check compares with, and no write of Dispatch's does it, while the result at the new head stands at that head;
  a closed issue takes a result for an `after_merge` check and still refuses a document write 409 `ISSUE_CLOSED`;
  a `default_branch` result stands at the branch for a commit the compare calls `behind` and at no basis for one it calls `diverged`;
  and a result whose issue resolves to a repository the installation lookup does not find is answered unknown, with the unreadable repository as the reason.
  Three more cases run the `dispatch` command as a process against that server.
  The command refuses a bare `-` given to any text flag its help lists but `dispatch artifact --path`, with exit 2 and a line that names the flag's `-file` twin.
  It keeps every other value of a text flag byte for byte.
  Run against a base binary (`DISPATCH_BIN`) or against the command of another tree (`VERIFICATION_RESULTS_E2E_DISPATCH_CLI`), it is the change's negative control

- `step-records-read.sh`

  the read, after the merge, of the step records of one issue a deployed Legion ran to done: `dispatch steps --issue <ISSUE>` run once in the operator's own shell with the operator's own credential, judged.
  It passes only when steps 1 to 5, the retro and the merge each have a record, every record has ended, every record of kind `step` or `retro` names an agent and every record of kind `step` a model;
  a read that lists no record, or that fails, is `UNKNOWN` and never a pass.
  A deployed Dispatch and the operator's credential; it starts nothing

- `plan-ratings-read.sh`

  the read, after the merge, of the plan and the ratings of one issue a deployed Legion ran to done: `dispatch plans --project <PROJECT>` run once in the operator's own shell with the operator's own credential, and the line of `<ISSUE>` judged.
  It passes only when the issue is listed with a plan and at least one rating, each with a reason, and prints the issue's time as `unknown` where Dispatch knows none;
  a read that lists no finished issue, or that fails, is `UNKNOWN` and never a pass.
  A deployed Dispatch and the operator's credential; it starts nothing

- `envoy-session-delivery.sh`

  every Envoy event reaches a real Oh My Pi session and a real Claude Code session under `legion claude-bridge` exactly once
  — agent sends and Dispatch messages, comments on a followed Dispatch issue (in their publish order), role publishes through the listener, core role publishes through the role lane, and a publish to the team topic both sessions follow
  — also when it was published while the session was killed and resumed, its NATS connection was cut, either of two listeners was killed, or Dispatch was SIGKILLed right after its writes, all built from the checkout;
  and a message stored on a session's own agent subject before that session first registers reaches it once;
  with `--build-from` an earlier revision's listener and plugins show the losses under a session restart.
  The `session-scripts` part of the `agent-harness` job runs it.

  `--serve` runs the same stack and leaves it up instead of driving the windows.
  It builds Dispatch's web app from the checkout and serves that build, gives both sessions a Dispatch-enabled `envoy.json` naming the run's own Dispatch and its agent token (a person's Send from the page becomes that person's own turn only after the session's plugin asks Dispatch to accept the delivery attempt), and prints the Dispatch URL, the Agents page, both session ids, the Claude Code session's `CLAUDE_CONFIG_DIR` and transcript, and the evidence directory, then holds until it is stopped.
  Dispatch runs with `DISPATCH_IDENTITY=header:X-Dispatch-User` and `DISPATCH_IDENTITY_HEADER_TRUSTED=1`, so the header alone signs a browser in:
  every request it makes must carry `X-Dispatch-User: live-delivery`, which Playwright sends as `extraHTTPHeaders`.
  `--log-model-requests`, which only `--serve` takes, puts a logging proxy in front of the model gateway `ANTHROPIC_BASE_URL` names and gives both sessions that proxy, so what a turn asks the model is evidence:
  one JSON line per request in `logs/model-requests.jsonl` under the evidence directory, holding the request's header names, its model and its thinking configuration, and never a header value or a message.

## Where each script runs

Every script here but two is a step of a job of `ci.yaml`, and that job gives it what it needs:

- `stage1-skeleton.sh` is a step of the `daemon-go` job ([In CI](#in-ci) says what the job gives it).

- `dispatch-claimant-delivery.sh` and `verification-results.sh` are steps of the `envoy-session-stall` job, each against a database of its own on that job's Postgres.

- `stage2-tmux-supervision.sh`, `controller-start-tmux.sh` and `clean-session-stop-tmux.sh` are steps of the `tmux-scripts` part of the `agent-harness` job;
  `dispatch-session-settings.sh` is a step of its `session-settings` part, `dispatch-user-turns.sh` and `envoy-session-delivery.sh` are steps of its `session-scripts` part, and `nats-callout-sessions.sh` is two steps of its `nats-callout-sessions` part, one in each of its modes.
  That job runs in the merge queue and on a pull request labelled `full-ci`.
  Each of these scripts takes Oh My Pi, and Claude Code, Codex CLI, Chromium or Postgres where it uses them, from the job, starts its own NATS and, where it calls a model, its own model stand-in, and prints one `CHECK` line for each check it makes.

- `leak-guard-image-probe.sh` is a step of the `worker-image` job, which runs it inside the image it has just built.

- `step-records-read.sh` and `plan-ratings-read.sh` are tools, not tests:
  an operator runs each after the merge against a deployed Dispatch, with the operator's own credential, and its header says for which decision.
  The `daemon-live` job's `TestPlanRatingsLive` runs `plan-ratings-read.sh` against the Dispatch it starts, which holds its parse to what a real Dispatch serves.

None of them needs a secret, and none may:
Sami answered that no API key goes into this repository, and that a smoke test which calls a model runs in the deployment repository on workload identity federation (sjawhar/legion#2512).
The whole-daemon tests that drive no agent are not scripts:
they are Go tests of `packages/daemon/internal/daemon` that the `daemon-live` job of `ci.yaml` runs by name on each pull request whose paths select it, and each test's own header says what it proves.
The whole-daemon tests that drive real Oh My Pi panes are Go tests of that package too, which the `agent-harness` job of `ci.yaml` runs by name in the merge queue and on a pull request labelled `full-ci`;
so is the Claude Code backend's test on tmux, `claude_backend_live_test.go`, which that job's `tmux-scripts` part runs.
The tests of the Sandbox runtime and of the whole daemon on a kind cluster are Go tests as well, which the `kind` job runs on the rig [`lib/kind-sandbox.sh`](#libkind-sandboxsh) builds.

## stage1-skeleton.sh

```sh
bash scripts/e2e/stage1-skeleton.sh     # → one "CHECK <name>: PASS" line per check, then "stage 1 e2e: PASS", exit 0
```

Each check it makes prints one line, `CHECK <name>: PASS`, or `CHECK <name>: FAIL: <reason>` for the first that does not hold, and the run exits 1 there.
A command that fails outside a check's own assertions names the check it ended, with its line and status.

Needs:

- `go`

- `docker`

- `jq`

- `curl`

- `python3`

- `tmux`

It builds the binary from the checkout (`packages/daemon/cmd/legion`), so it proves the tree you are standing in.
When the first tmux on `PATH` is a script or mise itself, the run's `PATH`, and so the daemon's, starts with the real binary that `mise which tmux` names in `packages/daemon`, the daemon's working directory ([`real_bin_path`](#librigsh) says why).
Such a machine needs tmux pinned in mise.
CI's tmux is the runner's apt binary, which the run leaves alone.

The daemon supervises its agents under tmux, and it refuses to start without what a launch needs
— tmux on `PATH`, an `operator_token_file`, and an OMP to run
— and before anything else its plugin gate holds the Oh My Pi plugin a pane would load to this daemon's contract (`packages/daemon/internal/daemon/bootgate.go`).
The run supplies all of it: its `legion.yaml` names a 0600 operator token file in the work directory;
`LEGION_OMP_PATH` points at a stub there that answers the gate's load probe (`omp models --extension <probe> --json`) as a pane loading both plugins does
— `LEGION_PLUGIN_LOADED=yes` and, beside it, `LEGION_PLUGIN_LOADED_FROM=file://…/dist/legion.js` inside the `@sjawhar/pi-legion` package the gate read and `LEGION_PLUGIN_ENVOY_INTERFACE=1`, then `LEGION_ENVOY_INTERFACE=1` (one number on both lines, whatever the plugins' own version:
the gate compares the two with each other, never with a constant) and `LEGION_ENVOY_LOADED_FROM=file://…/dist/envoy.js` inside an `@sjawhar/pi-envoy` package;
`LEGION_PROMPT_AGENTS=resolved` and `LEGION_PROMPT_SKILLS=resolved` when the gate asks for the task agents and skills the role prompts name;
and `LEGION_AGENT_MODELS=resolved` when it asks whether those agents' models resolve
— and exits 1 on anything else, because this proof launches no agent;
and the daemon runs with `HOME=<work>/home` and no `OMP_PROFILE`/`PI_PROFILE`, where `<work>/home/.omp/plugins/node_modules/@sjawhar/pi-legion/package.json` is the checkout's own manifest (`name`, `version`, `legion`), so it declares the contract this checkout's daemon requires, and `@sjawhar/pi-envoy/package.json` beside it owns the file the stub says the Envoy plugin loaded from.
The real gate against a real Oh My Pi is proven where a proof installs the plugins (`lib/install-plugin-profile.sh`); this one proves the skeleton.

Each item below names an input and its default, then what it means.

- `LEGION_E2E_PG_DSN` — unset

  the Postgres to run against.
  Unset, the script starts its own PostgreSQL 18 container (`legion-e2e-pg-<pid>`) on an ephemeral loopback port and removes it on the way out.
  Set, it starts no container: that is how CI hands it the job's service.
  Either way the run reads the DSN's host, port, user, password and database once, with Python's `urllib.parse`, and writes the DSNs of its first two steps from them;
  the server must check passwords, as CI's service and the container do, because the first step's wrong password must be refused.

Everything the run takes is its own, so two runs on one machine neither collide nor report each other as a leftover:

- a `mktemp -d` work directory (`$TMPDIR/legion-e2e.XXXXXXXX`, under `/tmp` where `TMPDIR` is unset)
  — the built binary, the three `legion.yaml`s (the daemon's, the wrong password's and the proxied daemon's), the operator token file and the OMP stub, the daemons' `home` with the plugin manifest, the two state documents, the refusal log, and in `logs/` each daemon's, keeper's and the proxy's log.
  Removed when the run passes; **kept when it fails**, and its path printed, because those documents are the evidence.

- `XDG_STATE_HOME=<work>/xdg` — so the legions registry the run writes is its own, never the box's `~/.local/state/legion/legions.json`.

- a per-run project key (`E2E<pid><epoch>`), and `E2E<pid><epoch>R` for the daemon behind the proxy
  — boots are counted per project, so a fresh key is what makes `boots == 1` true on a store that has served other runs, and the proxied daemon's boot is not counted in it.

- a daemon port the kernel chose, held for the whole run by a keeper ([`lib/rig.sh`](#librigsh)'s `keep_port`, [`lib/held-port.py`](#libheld-portpy)):
  `legion.yaml` names it, and each start serves on a copy of the keeper's socket (`LEGION_LISTEN_FD`, `start_kept`), so the restart serves the same port and nothing else can take it in between;
  the proxied daemon has a port and a keeper of its own, and the proxy a port the kernel chose;
  and, with no `LEGION_E2E_PG_DSN`, the container `legion-e2e-pg-<pid>`.

After each of the main daemon's two healthy starts, the first and the restart, the run checks that the socket the daemon serves on is held by the daemon's and the keeper's processes alone (`held_only_by`:
the socket's inode from the `LISTEN` row of `/proc/net/tcp`, its holders from `/proc/*/fd`):
a copy that leaked into the daemon's tmux server, a pane or a `legion` CLI call would keep the port listening after the daemon stops.

After each healthy start the run also reads `GET /readyz` and requires 200, the daemon's own account of having reached serving (the `servingLatch` the boot sets once its runtime is built, its launcher acceptor installed and the goroutine that serves this API begun), over a route that carries no token and reads no database.
It polls that read for 5 seconds, because the daemon sets the latch on a goroutine of its own that starts when the serving goroutine does, so `/healthz` can be answered in the instant before it runs;
a daemon that answers `/healthz` and never latches fails the run all the same.
It is read at the first boot and again after the restart, never at one of them alone:
the latch is one-way and the booting process's own, so every start begins unlatched, and `/healthz` is answered whether or not the latch was set — a daemon that answers `/healthz` while it never latches is the state this read catches, at either boot.

After any exit — pass, failure, or an interrupt
— the `EXIT` trap removes the container and signals the daemon, the proxy and the keepers;
`docker ps -a --filter name=legion-e2e-pg` comes back empty and nothing is left holding the run's ports.
A failed run prints each log of `<work>/logs` that holds anything: the two daemons' (`daemon.log`, `retry_daemon.log`), the proxy's and the keepers'.

### How it fails

Every step is fatal and names its reason:
a keeper that never held a port, a wrong password the daemon does not refuse, a refusal that does not name its host or that prints the password, a proxy that exits or holds no port, a first tmux that is a script or mise itself while `mise which tmux` names no tmux (`real_bin_path` refuses), a daemon that exits before it answers `/healthz` (the run stops at once and names the daemon's exit status; a daemon behind the proxy that exits at the first reset, as one that opens its store once does, stops it there), a `/healthz` that never answers (each poll times out after 2 s:
the kept port queues a request no daemon accepts), a `/readyz` that answers anything but 200 at either boot or behind the proxy (the failure names the status it got and the body with it, which says whether the daemon called itself starting or stopping), a daemon behind the proxy whose log does not hold exactly its two waits to open the store, attempt `1/10` with `retryIn` `1s` and `2/10` with `2s`, a socket held by any process but the daemon and its keeper, a state document that is not boot 1 with cap 4 and no issues, a registry without exactly one entry for this run, a `firstBootAt` that moved across the restart, a daemon that ignored a stop (SIGKILLed on the way out) or exited non-zero on one.
`stop_daemon` reads and judges the exit status, because `daemon.Run` returns 0 on a cancelled context — a non-zero status there is a defect, not a stop.

Two notes on what the script had to learn about its own surface:

- Postgres readiness is probed **over TCP** (`pg_isready -h 127.0.0.1`).
  The container entrypoint's bootstrap phase answers on the unix socket while nothing listens on 5432 yet, and a daemon that connects in that window is reset by the peer.

- The "never the password" assertion is written `grep -q … && exit 1`, not `! grep -q …`:
  `set -e` ignores a negated pipeline (shellcheck SC2251), so the negated form could never fail the run.

### In CI

On every pull request whose diff selects them, the `typecheck` job of `.github/workflows/ci.yaml` runs `go vet ./...` in `packages/daemon`, and its `daemon-go` job installs `tmux` — the tmux runtime's tests drive a real tmux server and skip without one, and the daemon refuses to start without it
— and the pinned `jj` (through mise, as the pi-legion job does)
— the workspace tests drive a real jj, and the daemon resolves jj at boot and refuses to start without it
— then runs `go test ./...` against its Postgres service (`LEGION_TEST_PG_DSN`, and `BROKER_TEST_DATABASE_URL` for the real broker that `internal/agentsecrets`'s contract tests drive, which fail there rather than skip without it), then this script with `LEGION_E2E_PG_DSN` pointing at the same service, so the script runs no docker of its own there.
`daemon-go` is the Go daemon's own job: that suite left the `test` job in dispatch://INFRA-1413, where one filter had to cover both it and the shell-script harnesses.

## stage2-tmux-supervision.sh

```sh
LEGION_TEST_OMP=<omp> LEGION_TEST_PG_DSN=<loopback postgres://…> bash scripts/e2e/stage2-tmux-supervision.sh     # → one CHECK line per check, then "stage 2 e2e: PASS", exit 0
```

The `tmux-scripts` part of `ci.yaml`'s `agent-harness` job runs it as a step, with the job's environment alone.
It needs:

- `go`

- `jq`

- `curl`

- `python3`

- `tmux`

- `socat`

- `bun`, with this checkout's dependencies installed

- `psql`

- `LEGION_TEST_OMP`, the Oh My Pi every pane and the boot gate run: the build `.omp-pin` pins, which `.github/actions/install-omp` installs and exports

- `LEGION_TEST_PG_DSN`, a loopback PostgreSQL the run creates its daemons' database in and drops on exit

Nothing else comes from outside the machine: no model, no key and no secret store.
What only this run proves is OMP's real RPC frames, `--resume`'s same-agent behaviour, the one-word `--append-system-prompt`, the plugin's parse of the Go daemon's answers, the plugin gate against an installed manifest, the provider-key path, a pane's model turn through the configured route, and the Envoy role claim.

What it stands up, all of it the run's own:

- **Postgres**: a database of its own on `LEGION_TEST_PG_DSN`'s server, which both daemons run on and the teardown drops.

- **NATS and the Envoy listener** on the host:
  `natsd` (`packages/envoy/internal/testnats/cmd/natsd`) built into the work directory and started with JetStream on a port it reports, and `packages/envoy`'s `cmd/listener` built there too and started with a fresh API bearer, which reaches every pane as the 0600 file `envoy_token_file` names, on a socket [`lib/held-port.py`](#libheld-portpy) opened on a port the kernel chose (`ENVOY_LISTEN_FD`).

- **The plugins**: this checkout's `@sjawhar/pi-envoy` and `@sjawhar/pi-legion`, each packed as the release packs it, installed into the OMP profile `legion-e2e2-<pid>-<epoch>` by `lib/install-plugin-profile.sh --omp-path "$LEGION_TEST_OMP"` (`--package pi-envoy`, then `--package pi-legion`), under the run's own Oh My Pi home `<work>/omp-home` ([`lib/omp-home.sh`](#libomp-homesh)).
  The daemons run with that `HOME` and with `OMP_PROFILE` naming the profile, so the gate and every pane load both, and the caller's `~/.omp/profiles` holds none of the run;
  `profile-stays-in-the-run` checks that last.

- **OMP**: `LEGION_OMP_PATH` set to `LEGION_TEST_OMP` in the main daemon's environment, which the daemon resolves to its real path for every boot probe and pane (`omplaunch.ResolveInvocation`).
  The script checks the boot log's resolved binary, the OMP child's `/proc/<pid>/exe`, and every command in the pane's first-child chain.

- **The model**: [`lib/messages-standin.ts`](lib/messages-standin.ts), an Anthropic Messages stand-in on loopback that answers every turn `ready` and refuses any request without the run's own placeholder key.
  [`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh) installs the key-file route into the profile with `--base-url` at the stand-in, keyed from a 0600 placeholder file of the run's own.
  Every model role of every pane is `anthropic/claude-opus-4-8`, and `anthropic` is the one provider a pane may use, keyed through `<work>/model-gateway-key/anthropic-key`, whose log records each call;
  no key reaches a pane, only the key command, which reads the file once per call.
  The stand-in records each request it answered or refused in `<work>/standin-requests.jsonl`.

- **The provider key**: `provider_keys: {GEMINI_API_KEY: file:<work>/gemini-key}`, a placeholder of the run's own, which proves the provider-key path;
  no pane's model uses it, and the profile disables the Google provider.
  The daemon reads the file at boot and writes it as a daemon-held 0600 file; every pane's shim exports it to OMP alone.
  `omp-child-environment` holds the value in OMP's environment to the file's.

- **The daemon**: `legion.yaml` with a fresh project key per run (`S2E<pid><epoch>` — a retired claim is never spawned again, so a reused key would fail on a store that served an earlier run), a port the kernel chose, held for the whole run by a [`lib/held-port.py`](#libheld-portpy) keeper, which each start serves on (`LEGION_LISTEN_FD`;
  the restart keeps it), as the second daemon's is by a keeper of its own, `probe_interval_seconds: 5`, and the operator token file `legion claims` presents.
  `XDG_STATE_HOME` and `TMUX_TMPDIR` point into the work directory, so the legions registry and the private tmux servers are the run's.

The checks, in order, each printing what it observed (`== <check>` … `CHECK <check>: PASS`), after a `CHECK setup: PASS` for the rig itself:

| check | what it does and requires |
| :--- | :--- |
| `gate-refuses-another-contract` | edits the installed (unpacked) `pi-legion` manifest to declare the next `daemonApiVersion`; `legion start` refuses naming both numbers; the manifest is put back byte for byte |
| `gate-refuses-a-disabled-plugin` | `omp plugin disable @sjawhar/pi-legion`; `legion start` refuses with "installed but not loaded by omp"; `omp plugin enable` |
| `gate-refuses-without-the-envoy-plugin` | `omp plugin disable @sjawhar/pi-envoy`; `legion start` refuses with "but no pi-envoy is: install the @sjawhar/pi-envoy release", naming the package to install before any skill it ships is reported missing; `omp plugin enable` |
| `gate-refuses-a-missing-skill` | the installed plugin's `dist/skills/thermonuclear-deep-review` moved aside; `legion start` refuses with "finds no skill thermonuclear-deep-review (loaded by agents/thermonuclear-deep-review.md, roles/core/reviewer.md)", the agent definition and the reviewer's role prompt that load it; the rubric put back |
| `gate-refuses-a-skill-only-a-role-prompt-loads` | the installed plugin's `dist/skills/legion-controller` moved aside, a skill the daemon's `roles/controller-root.md` loads; `legion start` refuses with "finds no skill legion-controller (loaded by …)", naming the pi-legion files that load it and then `roles/controller-root.md`, which only a gate reading the daemon's own role prompts can say; the script reads that list from the installed plugin and the checkout's role prompts as the gate does (`gate_loaders`), and fails when `roles/controller-root.md` is not in it; the skill put back |
| `gate-refuses-an-unconfigured-model-role` | `modelRoles.oracle` removed from the isolated profile's `config.yml`; `legion start` refuses with "on its model @oracle: role oracle is not configured", naming `task agent oracle` and the prompts that dispatch it; the profile put back byte for byte |
| `architect-registers-and-is-ready` | `legion start` passes the gate (its log line, `boot gate: pi-legion speaks this daemon's contract and loads with pi-envoy`); `legion claims spawn` of a root architect whose role prompt says to reply `ready` and wait; the claim reaches `ready` and the daemon logged its registration at contract 1 |
| `envoy-role-held` | `GET /v1/roles/<claim token>` on the listener names the claim's session as holder within 60 s (the plugin takes the name at the session's first turn, or 30 s after `claims/ready` when none starts, as for this task-less architect) |
| `ready-in-state` | `legion state --json` shows the issue's architect `ready`, with that session and a tmux locator |
| `task-queued-before-ready-runs-once` | a second claim spawned with a task: the spawn's answer holds the delivery unsent while the claim is launching; it ends `idle` with nothing pending, and OMP's session file holds the task once |
| `model-turn-through-the-route` | every assistant turn in that claim's session file ran on the `anthropic` provider as the profile's pinned model (`anthropic/claude-opus-4-8`), none ending in an error; the stand-in answered a model request of the task with the route's key and refused none of it; and the key command served more calls than its preflight, which records none |
| `retried-frame-starts-no-second-turn` | stops the claim's shim (SIGSTOP) and delivers a task: every send goes unanswered, and the daemon sends the same delivery id again at the next sweep (`the prompt was lost to the transport` twice in its log); resumed, the shim hands OMP the first frame and answers the repeat from its record — the session file holds the task once, and no prompt failure is charged |
| `kill-pane-resumes-the-same-session` | `tmux kill-pane` on the architect: the next generation is ready in a new pane with the same session, its OMP started with `--resume=<session file>`, and the Envoy role still held |
| `suspend-keeps-the-session` | `legion claims suspend`: `suspended`, no locator, the pane gone, the session and its file kept |
| `resume-on-demand` | `legion claims resume`: `ready` at the next generation in a new pane, same session |
| `stale-generation-hello-refused` | writes a hello carrying generation 1's boot token to `<state_dir>/worker-stream.sock`: the daemon closes it with nothing written and logs `rejected hello (stale worker generation)` |
| `unregistered-agent-retired-at-the-deadline` | a second daemon whose `LEGION_OMP_PATH` stub answers the plugin gate and the boot's `config get retry.modelFallback --json`, and otherwise sleeps, with a 10 s registration deadline (5 s × 2): the claim's shim connects and its process lives, the agent never registers, and at the deadline the process is retired and one launch failure counted |
| `restart-readopts-the-live-panes` | SIGTERM, then start again: `boots` +1, both claims keep their generation, incarnation and pane, no pane opens or closes, and a task delivered after the restart runs once — over the connection the shim's reconnect hello opened |
| `omp-child-environment` | the boot log names the resolved OMP binary for both probes and panes; the pane's process is `/bin/sh -c`; the one process below it that runs that exact executable is the pane's OMP ([`launched`](#libprocess-treesh)), and no process on the line up from it to the pane's shell runs the OMP wrapper that remains first on the ordinary daemon PATH; the pane's shell and that OMP carry all four XDG base directories under `<state_dir>/home` and no `DBUS_SESSION_BUS_ADDRESS` (LEGION-206 P1); OMP carries `GEMINI_API_KEY` as the provider-key file holds it and its shim does not, and no `ANTHROPIC_API_KEY`, `GEMINI_API_KEY_TESTS`, `SOPS_AGE_KEY_FILE` or `SECRETSD_CONFIG` |
| `stray-pane-reaped-after-the-grace` | opens a window marked as the daemon's (`@legion_owner`) holding no recorded pane, and an unmarked one beside it: the periodic orphan sweep (every 60 s, 120 s grace) reaps the marked one no sooner than 120 s after it opened, and keeps the unmarked window and both claims' panes |
| `stop` | `legion claims stop` of the root architect is refused — 409, "the tree's root claim ends only when its tree closes; suspend it to stop its process; no workflow issue backs its tree, so legion claims close ends it" — and moves nothing: its state, generation, and pane are what they were; `legion claims suspend` suspends the root; `legion claims stop` of a second worker, S2-3's tester, retires it and its pane is gone; no workflow issue backs S2-1, so `legion claims close` of its root closes the tree while its first worker is still live: the root and the worker are both retired and the worker's pane is gone; `legion stop` ends the daemon with exit 0 |
| `every-turn-through-the-route` | [`lib/check-model-route.sh`](#libcheck-model-routesh) over every session in the isolated profile, each subagent's included: every assistant turn was served by the `anthropic` provider, the route's; and its negative control, a copy of one captured session with a turn rewritten as Bedrock's, is refused |
| `profile-stays-in-the-run` | every session file is under the run's own home, and the caller's `~/.omp/profiles` holds no directory of the run's profile |

Every wait is bounded and names what it waited for;
a failed assertion prints `CHECK <check>: FAIL: <why>` and exits 1, and any other failing command names the check it ended the same way.
A failed run also [notes](#libmodel-gateway-unservedsh) each agent that could have failed that check for want of a model key, and still exits 1.
The `EXIT` trap — on a pass, a failure, or an interrupt
— stops both daemons (SIGKILL after 10 s), kills both private tmux servers, stops the listener, natsd and the stand-in, ends every process of the run still alive with everything under it ([`stop_marked_processes`](#libprocess-treesh): each that carries the run's mark, `LEGION_E2E_RUN`, or a daemon pane's `XDG_CONFIG_HOME` under the work directory, and no other), drops the run's database, and removes the work directory when the run passed (keeping it, with `transcript.log` (the whole run), `daemon.log`, `deadline.log`, `listener.log`, `standin-requests.jsonl`, the model key command's log and each refusal's log, when it did not).
The trap still runs when whoever reads the run's output goes first, for example a supervised launcher's own `tee` stopped with the run, and a stop follows:
the run's output goes through [`lib/transcript.sh`](#libtranscriptsh)'s `tee`, which outlives the reader.
The transcript's `tee` starts before the run exports its mark, so it carries none, and the teardown's sweep, which ends only the run's marked processes, leaves it running to write the run's last line.

Three things the run had to learn about its surface:

- The shim is a Go process, which starts its child from whichever thread runs the goroutine:
  the first-child walk reads `/proc/<pid>/task/*/children`, never the main thread's list alone.

- OMP takes the session to resume as `--resume=<file>`, one argument.

- The shipped orphan sweep is the only reaper after boot (boot's own reconcile runs with grace 0), so the stray is opened after the restart and the check waits up to five minutes.

## controller-start-tmux.sh

Task 4b.5's acceptance for `legion controller start`, on tmux.
The Go daemon runs on a real Postgres with this checkout's two plugins in an isolated OMP profile, against a real Envoy listener and NATS, and the command runs in real tmux panes, as an operator would.
The `tmux-scripts` part of `ci.yaml`'s `agent-harness` job runs it as a step.
Its Oh My Pi is `LEGION_TEST_OMP`, the build `.omp-pin` pins, which `.github/actions/install-omp` installs, named to every `legion` of the run as `LEGION_OMP_PATH`;
its Postgres is a database of its own on `LEGION_TEST_PG_DSN`'s server, which it drops on exit;
its NATS is `natsd` (`packages/envoy/internal/testnats/cmd/natsd`), built from this checkout;
and the real gh every process of the run is told of (`LEGION_GH_PATH`) is the runner's `/usr/bin/gh`.
The run needs no model route: the controller's one model turn, the start message (LEGION_CONTROLLER_START_MESSAGE) that pi-legion's `/legion-controller-start`, the word the command puts on Oh My Pi's command line, sends once Oh My Pi's startup is over, fails against the profile's default, a static-key provider that listens nowhere, and no check reads its answer.
The profile names the roles the task agents use and that default, which is all the boot gate's agent-model check resolves.
Its `config.yml` also shuts Oh My Pi's first-run setup wizard ([`omp_setup_wizard_off`](#libomp-homesh)), and each controller pane's command sets `OMP_SKIP_SETUP=1`, which the pinned build reads, since the wizard would otherwise hold both controller panes, so `controller-claims-the-role` would time out with the wizard's `Sign in to your providers` step on the screen.
The liveness check runs the daemon's own `controller.Prober` through [`lib/controller-liveprobe.go`](lib/controller-liveprobe.go), a main the run overlays into `packages/daemon` with `go run -overlay`.

```bash
LEGION_TEST_OMP=<omp> LEGION_TEST_PG_DSN=<loopback postgres://…> bash scripts/e2e/controller-start-tmux.sh
```

Each check prints `== <name>`, what it observed, and `CHECK <name>: PASS`.
The first check that fails prints `CHECK <name>: FAIL: <why>` and ends the run with exit 1.
On any exit the run removes its scratch directory, the isolated profile and both tmux servers, stops natsd and drops its database, also when whoever reads the run's output goes first and a stop follows:
the run's output goes through [`lib/transcript.sh`](#libtranscriptsh)'s `tee`, which outlives the reader.
`CONTROLLER_START_EVIDENCE_DIR` keeps `transcript.log` (the whole run), the daemon, natsd and listener logs and, under `checks/`, each check's own output (the controllers' stderr and exit codes, the refusal, the route answers, the prober's log) and both controller panes as they were at exit, so a failed run keeps what failed;
it defaults to a fresh `/tmp` directory, which is printed.
No secret is written there.

Each item below names a check, then what it holds, with 4b.5's acceptance item in parentheses.

- `config-check-passes`

  `legion config check` passes the run's tmux config:
  its first line is the `Config OK` line, and the lines after it are exactly the two deployment capabilities a tmux file alone leaves open (`capability secrets is open: …` then `capability resource-limits is open: …`, each ending with its `capabilities.decided.<name>: "<reason>"` line) and then `pause unavailable: runtime: tmux`, the setting under which this daemon cannot pause its project, a report that keeps the exit 0 (LEGION-578).
  4b.5's item that the check passes on a `runtime: kubernetes` config is the kind job's whole-tree test's, whose daemon loads its file through the same loader (`TestStage4bSandboxTreeOnTheKindRigLive`'s `advertise-host`)

- `config-check-names-each-broken-key`

  an unknown key, a zero `admission_cap`, a bad `envoy_url`, and a Dispatch URL without its token file are each refused by name, and neither App's `private_key_command` runs (names the key on each broken variant)

- `gate-refuses-the-previous-contract`

  the boot gate refuses the plugin with its manifest set to the contract before the checkout's, naming both contracts (the plugin at the bumped contract refuses the previous one by name)

- `daemon-serves`

  the daemon answers `/healthz` with no `controllerLocator` yet

- `state-config-runs-no-key-command`

  `legion state --config` on a config whose key command would fail still reads the state, and the command never runs (`legion state --config` runs no key command)

- `operator-token-file-others-can-read-is-refused`

  a 0640 operator token file is refused by path and mode, the daemon mints nothing, and no state directory is written (a group-readable operator token file is refused naming it)

- `controller-claims-the-role`

  the controller registers, `GET /legion/v1/state` shows `controllerLocator` (`runtime: tmux`, `external: true`), the Envoy role `legion-<project>-controller` names its session, and its Oh My Pi has the controller environment, its secret only as a 0600 file, interactive (claims the controller role, shows `controllerLocator`);
  with no GitHub App configured, its stderr says the controller's gh acts as nobody, and `$ctl_state/gh` is an empty 0700 directory with no credential log.
  That Oh My Pi is `controller_omp`'s: the process below the pid the pane recorded for `legion controller start` ([`record_pid_sh`](#libprocess-treesh)) that runs the pinned Oh My Pi, whose environment is read once, with no pipe.
  `lib/controller-start-guards.test.ts` (`bun test scripts/e2e/lib`) runs `controller_omp` against a stand-in `legion` that runs its Oh My Pi through `sh -c`: it finds that Oh My Pi twenty times of twenty when it runs under another name, and when a mebibyte of its environment follows `LEGION_STATE_DIR`, and refuses one of another state directory.
  Against the lookup before this one, which read every process of the machine and took the first whose `argv[0]` was named `omp` and whose environment, read through `tr | grep -q`, named the state directory, both of the first two rows find it 0 times of 20

- `ctrl-c-reaches-omp-not-the-cli`

  one Ctrl-C leaves both `legion controller start` and Oh My Pi running (decision 3: Oh My Pi owns the terminal)

- `status-from-an-operator-shell`

  `legion issue status --operator-token-file` mints a grant with the operator bearer and reaches the status route;
  the rig has no Dispatch, so the route answers 500 naming it.
  A wrong bearer is refused 403 (decision 3's operator path; the status write itself is the full-tree proof's)

- `second-start-revokes-the-first`

  a second start takes the role and the locator, the daemon logs mints 1 and 2, the first capability's registration is refused 403, and a grant minted before the second start, which redeemed then (500 `DISPATCH_UNAVAILABLE`, past the grant check), is refused 403 `GRANT_UNAVAILABLE` after it (a second start revokes the first's capability)

- `liveness-probe-against-the-listener`

  `controller.Prober` on the live listener calls the second session `live` and the first `gone`

- `exit-code-is-oh-my-pis`

  Ctrl-D quits Oh My Pi cleanly and the command exits 0, as Oh My Pi did.
  A non-zero code is carried through too; the stub-omp unit test (`cmd/legion/controller_test.go`, exit 3) holds that

## dispatch-user-turns.sh

LEGION-394's acceptance: a person's direct Send or Aside from Dispatch's conversation page is the session's own user turn, and everything else keeps its Envoy card.
One real session — the Oh My Pi `LEGION_TEST_OMP` names (the build `.omp-pin` pins) with this checkout's Envoy plugin in an isolated profile, in a tmux pane, its cwd under `/tmp` — registers with a real Envoy listener and natsd, built from this checkout.
The profile's model route shuts Oh My Pi's first-run setup wizard ([`omp_setup_wizard_off`](#libomp-homesh)), which a fresh profile's first launch otherwise sits in.
Dispatch, built from the checkout with NATS on and its trusted identity header and on a database of the run's own in `LEGION_TEST_PG_DSN`, serves the SPA this checkout builds, and Playwright drives the conversation page in its own Chromium as the person the header names.
The session's model is [`lib/user-turns-model.ts`](lib/user-turns-model.ts), a stand-in on [`lib/messages-standin.ts`](lib/messages-standin.ts) whose scripted answers make the tool calls the spec-edit checks need (`dispatch issue`, then `dispatch doc-edit` after the person's edit) and name the code word each prompt carries.
The session reaches it through the key-file route ([`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh) `--base-url`), keyed by a placeholder of the run's own.
The agent-harness job's `session-scripts` part runs it as a step.

The rig serves the checkout's built `packages/dispatch/web/dist` live.
Do not run `bun run build:web` or `bun run e2e` in this checkout while a run is live:
either rewrites that directory under the Dispatch process and can leave its page with stale asset names.
The profile also holds what the slash-command checks send, each named with the run's token:
a skill (`skills/<name>/SKILL.md`), a prompt template (`prompts/<name>.md`), a file command (`commands/<name>.md`), an extension (`extensions/<name>.ts`) registering a command that writes its arguments to `checks/extension-command-args` and appending the role, `customType` and `tag` of every `message_start` the host hands extensions to `checks/message-starts.jsonl`, and `compaction.keepRecentTokens: 200` appended to the `config.yml` the model route wrote, so the run's one `/compact` has history to summarize.
Each slash-command check waits until the session is idle before it sends, since the page refuses a send while a turn runs:
a stopped answer with no message or custom message after it and no background job the model started still owed its result.

```bash
LEGION_TEST_OMP=<omp> LEGION_TEST_PG_DSN=<loopback dsn> bash scripts/e2e/dispatch-user-turns.sh     # → "dispatch user turns e2e: PASS", exit 0
LEGION_TEST_OMP=<omp> LEGION_TEST_PG_DSN=<loopback dsn> DISPATCH_USER_TURNS_ONLY=spec-edit bash scripts/e2e/dispatch-user-turns.sh   # the spec-edit checks alone
```

| input | default | meaning |
| :--- | :--- | :--- |
| `LEGION_TEST_OMP` | required | the Oh My Pi the session runs |
| `LEGION_TEST_PG_DSN` | required | a loopback PostgreSQL with pgvector; the run creates Dispatch's database there and drops it on every exit |
| `DISPATCH_USER_TURNS_ONLY` | unset, every check | `spec-edit`: `session-registers` and the two spec-edit checks alone, then PASS; `spec-edit-card`: `session-registers` and `a-persons-spec-edit-reaches-its-writer` alone, then PASS |
| `DISPATCH_USER_TURNS_EVIDENCE_DIR` | a fresh `/tmp/legion-e2e-user-turns-evidence.XXXXXXXX` | kept on every outcome and printed at exit: `logs/` (the listener, Dispatch, natsd, the stand-in and its requests, and the SPA build), `checks/` (the page's screenshots, the session's pane at exit, `extension-command-args` and `message-starts.jsonl`) and `session.jsonl`, the session's transcript |

Each check prints `== <name>`, what it observed, and `CHECK <name>: PASS`.
The first check that fails prints `CHECK <name>: FAIL: <why>` and ends the run with exit 1.
On any exit the run removes its scratch directory, the isolated profile under the HOME it gives Oh My Pi, its tmux server, the stand-in, natsd, its listener and Dispatch, and drops Dispatch's database;
a run that finds another's leftovers refuses to start ([`lib/leftovers.sh`](#libleftoverssh)).

| check | what it holds |
| :--- | :--- |
| `session-registers` | the session registers with the listener from its cwd and advertises `aside`, `btw` and `steer` |
| `a-persons-spec-edit-reaches-its-writer` | a person's message asks the session to create an issue in project UT whose spec's first section says `The export runs at 02:00 UTC.` and whose second section repeats the time, so the session is the spec's creator; the page opens the issue's Spec tab as the person, its document's connection dot reads connected (so the trusted header reaches the editor's socket), and it selects the first section's sentence and types `The export runs at 04:00 UTC.` over it; once the person's `artifact.version` settles, with Dispatch's own 30 s quiet time, the session's transcript holds one `artifact.human_edited` card whose diff adds the new sentence, within 60 s |
| `the-writer-rewrites-what-the-edit-touched` | within 5 minutes of the person's version, a later version that lists the session says `04:00` in both sections and `02:00` nowhere |
| `send-is-the-persons-own-turn` | the page's composer opens on Send and offers Send, Aside and BTW; a Send typed there is one user message in the session's transcript that is exactly the body, and no card; the session answers it |
| `aside-is-the-persons-own-turn` | the same for an Aside |
| `btw-is-a-side-question` | a BTW is answered in Dispatch and is no user message |
| `a-session-forging-a-person-gets-a-card` | holding the listener token and a Dispatch bearer, as any session does, the driver stores a session's message on an issue and publishes a frame naming it that claims a person wrote it, on no issue: a card, no user message |
| `a-broadcast-gets-a-card` | a person's broadcast to the session: a card, no user message |
| `an-issue-message-gets-a-card` | a person's issue message targeted at the session: a card, no user message |
| `a-legion-notice-gets-a-card` | the Go daemon's `phase-finished` notice on a role topic the session holds: a card, no user message |
| `a-session-re-sending-a-persons-btw-gets-a-card` | as any bearer may, the driver retries the person's BTW to the session as a steer (`POST /api/v1/messages/{id}/deliveries` with a session actor), an attempt whose `requested_by` is that session: it arrives as a card, no user message (the refusal itself is `message_accept_test.go`'s) |
| `a-carded-send-and-a-frame-forged-for-it-inside-the-minute-get-cards` | with the session's Dispatch token file made wrong, the person's Send arrives as a card, since the session cannot accept it; with the token restored, a frame forged with the listener token names that attempt within 45 s of the Send, while Dispatch would still accept it: the session's own record of the attempts it delivered keeps it a card, with neither the Send's text nor the forged text a user message, and Dispatch records no acceptance |
| `a-persons-retry-of-a-carded-send-is-their-turn` | the person retries that Send as an Aside (`POST /api/v1/messages/{id}/deliveries`, attempt 2), an attempt the session never delivered: one user message that is exactly the body, and Dispatch records attempt 2 accepted |
| `the-composer-lists-the-sessions-commands` | typed key by key in the composer, `/rig` lists the rig's template, file command and extension command, `/skill:rig` its skill, and `/sess`, `/comp` and `/new` the built-ins `session`, `compact` and `new`, `new` alone marked terminal only; nothing is sent |
| `a-forged-command-list-changes-nothing-on-the-page` | with a page open, a bare bus client publishes five malformed `commands` frames (an unknown `source`, `commands` not a list, an empty name, a whitespace name, `v` 2) on `agentstream.<session>.frames`, each at a sequence number above any the session has reached (the run's clock in the publisher's unit, a thousand a millisecond): the page's list stays the session's and no error screen shows; a well-formed list published after them replaces it, so the frames reached the page; a fresh page lists the session's own again |
| `a-skill-runs-as-the-skill` | `/skill:<name> …` picked from the list and sent: one `skill-prompt` custom message tagged with the Dispatch message id, one `message_start` for it that the rig's extension saw (the host event Legion's phase-stall check reads), no user message holding the raw text, and the session answers with the skill's code word |
| `a-template-and-a-file-command-run-as-their-expansions` | each picked and sent with an argument: one user message tagged with its Dispatch id, holding the expansion with the argument substituted rather than the raw `/name`, and the session answers it |
| `an-extension-command-runs-its-handler` | the extension's command picked and sent with arguments: its handler wrote exactly those arguments to `checks/extension-command-args`, and no user message was submitted |
| `session-replies-with-its-id` | `/session`: the first line of the session's Dispatch reply to it is `Session: <session id>` |
| `compact-compacts-and-replies` | `/compact`, sent once, after the checks above gave the session history: the reply starts `Compaction complete`, and the transcript gains a `compaction` entry |
| `new-is-terminal-only` | `/new`: the reply says it runs only in the session's own terminal and nothing was sent, and the listener lists the same session with the same user messages |
| `an-unknown-command-is-the-persons-words` | a Send starting `/foo<run>`, which no command names: one user message, the body alone, tagged with its Dispatch id, and the session answers it |
| `the-send-and-aside-carry-their-dispatch-ids` | the Send's and the Aside's user messages each carry their Dispatch message id as their `tag` in the session's transcript |
| `the-page-shows-each-message-once` | a fresh page, opened while the session's stream still holds the turns it tagged (the ring lives in the process, so the restart below empties it): the replay the page is served carries the Send, the Aside and the retried Send as user messages tagged with their Dispatch message ids, beside their stored copies, and the page shows each person's message once |
| `the-session-stops` | the Send's own envelope is read back from the notification stream; Oh My Pi is killed and the listener drops the session |
| `a-send-while-the-session-is-down-fails` | the person sends the stopped session a Send, which Dispatch records as one failed attempt naming it |
| `a-replay-after-restart-gets-a-card` | once Oh My Pi is continued (`--continue`, the same session), the Send's own envelope is sent again: a card, and the Send is still one user message |
| `a-frame-forged-after-the-restart-naming-an-old-send-gets-a-card` | once the Send made while the session was down, which Dispatch recorded as failed, is more than a minute old, a frame forged with the listener token names it as a person's steer: it arrives as a card, with neither its stored nor its forged text a user message |
| `a-frame-forged-for-a-failed-send-inside-the-minute-gets-a-card` | the listener drops the session's registration (`DELETE /v1/sessions/{id}`), so the person's Send to it fails with no frame sent, and within 45 s a bare bus client publishes a frame naming that failed attempt straight onto the session's agent subject: it arrives as a card, with neither text a user message |
| `dispatch-records-only-the-turns-the-session-took` | Dispatch's thread read, which the Agents page reads, records the Send and the Aside accepted at attempt 1 and the carded Send at its retry, attempt 2, and never the BTW, the Send made while the session was down or the Send made while the listener listed no session |

## dispatch-session-settings.sh

The local-stack proof of the live view's settings.
Dispatch's live view shows a real Oh My Pi session's current model and thinking level and switches them from its header.
The stack is real: Oh My Pi with this checkout's Envoy plugin, an Envoy listener, natsd, Postgres, Dispatch built from the checkout, and its page driven in Playwright's own Chromium.
Only the models are stand-ins, and the agent-harness job's `session-settings` part runs it as a step.
The rig never takes the page as its own oracle.
Each value the header shows is compared with what the session reports without the page:
RPC `get_state` (the model) and `get_available_models`, the `Thinking:` and `Available:` lines an `/effort` prompt prints (RPC `command_output`), the session file's `model_change`, `thinking_level_change`, card and user-message entries, and Dispatch's stored messages, read with the person's header.

```bash
LEGION_TEST_OMP=<omp> LEGION_TEST_PG_DSN='postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable' \
  bash scripts/e2e/dispatch-session-settings.sh     # → "dispatch session settings e2e: PASS in <n>s", exit 0
```

The script does only the setup and the cleanup.
The driver, [`lib/dispatch-session-settings.ts`](lib/dispatch-session-settings.ts), owns every Oh My Pi process and one held-open page, signed in as `alice`, and runs the checks in order, each its own function.
The parts it drives are its siblings in `lib/`:
[`session-settings-omp.ts`](lib/session-settings-omp.ts) (Oh My Pi in `--mode rpc`), [`session-settings-services.ts`](lib/session-settings-services.ts) (Dispatch, the listener and the bus), [`session-settings-browser.ts`](lib/session-settings-browser.ts) (the held-open page), [`session-settings-session-file.ts`](lib/session-settings-session-file.ts) (the session file's entries) and [`session-settings-common.ts`](lib/session-settings-common.ts) (the rig's setup, the transcript and the waits).

It differs from [`dispatch-user-turns.sh`](#dispatch-user-turnssh) in these ways:

- **It needs no docker, tmux, mise or root.**
  It uses `go`, `bun`, `jq`, `curl`, `psql`, `tar` and `python3`, and Playwright's Chromium, which `bunx playwright install chromium` in `packages/dispatch` downloads.

- **Oh My Pi runs in `--mode rpc`, as a Legion pane runs it, not as the TUI in tmux.**
  The command is `omp --extension <pi-envoy> --mode rpc`, with the plugin packed by [`lib/pack-plugin.sh`](#libpack-pluginsh) and unpacked as the worker image unpacks `/opt/legion/pi-envoy`.
  The driver reads and writes the RPC protocol on the process's stdin and stdout.
  The profile is the agent directory under a `HOME` of the run's own, which the driver writes with `writeStandinProfile` (`packages/pi-shared/test/omp-harness.ts`).
  That `HOME` links `.omp/natives` to the caller's cache, as [`lib/omp-home.sh`](#libomp-homesh) does.
  The Oh My Pi is `LEGION_TEST_OMP`, not a mise tool spec.

- **Every model is a stand-in.**
  [`lib/session-settings-model.ts`](lib/session-settings-model.ts), on [`lib/messages-standin.ts`](lib/messages-standin.ts), serves an Anthropic-shaped provider and an OpenAI-shaped one, keyed by a placeholder of the run's own that the profile names.
  It answers each one-word prompt with its word and the long prompt of `picker-during-turn` with a call of the session's bash tool.

- **Postgres is a loopback server the caller names**, in `LEGION_TEST_PG_DSN`.
  The script creates a database of its own in it with `psql` and force-drops it with `psql` on any exit.
  It takes the DSN only when [`lib/loopback-dsn.py`](#libloopback-dsnpy) takes it, through [`lib/loopback-dsn.sh`](#libloopback-dsnsh)'s `run_database_url`, which it calls in `setup`, before it builds or starts anything.
  Dispatch's `DATABASE_URL` is the connection string `run_database_url` rebuilds from the keywords libpq reads of the DSN, with that database as its `dbname`, so no client of the run is handed the DSN's own text, and `setup` refuses a DSN the rebuild cannot carry.
  psql is libpq, and Dispatch reads that rebuilt string with pgx (`pgxpool.ParseConfig`), which reads the keyword/value form as libpq does: the pinned `github.com/jackc/pgx/v5 v5.11.0` carries a port of libpq's parser, and `lib/loopback-dsn.sh` says what a raise of that pin moves.

- **NATS is natsd** (`packages/envoy/internal/testnats/cmd/natsd`), built from this checkout.
  It starts with JetStream on a port the kernel picks.

- **No variable of the caller's shell named `DISPATCH_*`, `ENVOY_*`, `NATS_*`, `OMP*`, `PI_*`, `PGHOSTADDR`, `PGSERVICE` or `PGSERVICEFILE` reaches any process of the run.**
  A Legion pane's own Dispatch, Envoy and profile stay out of it, and the three `PG` variables stay out of every psql and every client the run starts.

**The model list `picker-refusal` picks from is injected, not reported by the session.**
The check makes the session refuse a model because it has no key for that model's provider.
Under the run's keys, the session lists no such model, so its header cannot offer one.
A bare NATS client publishes a `settings` frame on `agentstream.<session>.frames`.
The frame's `seq` is above the session's own, and it lists the session's own models plus `standin-unkeyed/unkeyed-model`, a model of a provider the profile holds no key for.
The person picks that model.
The refusal comes from the real session, and the page shows it.
The check then reloads the page, so that the session's next replay puts its real frame back.
Any process that can reach NATS can publish this frame.
That is the frames subject's existing exposure, the one `dispatch-user-turns.sh`'s `a-forged-command-list-changes-nothing-on-the-page` drives.

The two models are `standin-anthropic/standin-claude`, the profile's default and the fallback target, and `standin-openai/standin-gpt`.
Their `/effort` lists differ, and the preflight refuses two models whose lists are the same.
The first offers `off, auto, low, medium, high, xhigh`, and the second `off, auto, minimal, low, medium, high`.
Before any check, the preflight runs a one-word prompt on each model, in a session with no plugin.

The rig serves the checkout's built `packages/dispatch/web/dist` live.
Do not run `bun run build:web` or `bun run e2e` in this checkout while a run is live, for the reason `dispatch-user-turns.sh` gives.

Each item below names an input and its default, then what it means.

- `LEGION_TEST_PG_DSN` — required

  a loopback PostgreSQL with pgvector; the run makes and drops its own database there, and takes the DSN only when [`lib/loopback-dsn.py`](#libloopback-dsnpy) takes it

- `LEGION_TEST_OMP` — required

  the Oh My Pi to run.
  The rig refuses a host whose first replay has no `commands` frame, because that host cannot run typed input from Dispatch

- `SESSION_SETTINGS_EVIDENCE_DIR` — a fresh directory under `TMPDIR`

  kept and printed: `transcript.log`; `checks/`, one screenshot per check;
  `logs/`, with the builds, the services, each Oh My Pi's stderr and RPC events (`omp-<label>.rpc.jsonl`), the Oh My Pi logs (`logs/omp/`) and the profile's `config.yml` and `models.yml`; and `sessions/`, each session file

The profile's `config.yml` sets `defaultThinkingLevel: low` and every `modelRoles` entry to the first model, so no side request (a title, a summary) goes to another model.
It also sets `retry.fallbackChains` (the key the fork's `session/retry-fallback-chains.ts` reads) from `rig-broken/broken-model` to the first model.
`models.yml` adds the provider `rig-broken`:
a literal key and one model, `broken-model`, whose `baseUrl` is a loopback port that a `python3` process of the run binds and never listens on.
Every connection to that port is refused.

Each check prints `== <name>`, what it observed, and `ok <name>`.
The first check that fails ends the run non-zero and names itself.
Check 2 of the plan is three checks here, so a passing run prints 20 `ok` lines.
On any exit, the script stops what it started by the pid it started:
the `nats-server`, the listener, Dispatch, the port holder and every Oh My Pi in `$work/omp.pids`.
It stops an Oh My Pi only while that pid's command line still names the run's work directory.
It also drops the database and removes the work directory.

Each item below names a check, then what it holds.

- `header-before-any-turn`

  before any prompt, the header shows `get_state`'s model and the level on `/effort`'s `Thinking:` line.
  The model picker offers exactly what `get_available_models` lists, and the thinking picker offers `/effort`'s `Available:` list, in its order.
  No turn has run in the session file or on the page

- `terminal-set-model`

  RPC `set_model` to the second model, then back to the first.
  The same open page shows each within 20 s of the RPC answer, `get_state` agrees, and the session file gains no user message

- `terminal-set-thinking`

  RPC `set_thinking_level medium`.
  The page shows it within 20 s, `/effort` reports it, and no user message is added

- `terminal-typed-effort`

  an RPC `prompt` of `/effort high`, the host's own typed route.
  It prints `Thinking set to high.`, the page shows `high` within 20 s, and no user message is added

- `message-switch`

  the composer sends `/model <second>` as a Send.
  The header shows it within 20 s, and the session replies `Model set to <second>.`

- `picker-model`

  a pick of the first model.
  Dispatch stores `/model <first>` authored by `alice`, the thread shows it as hers, and the session's reply `Model set to <first>.` shows on the switch line and in the thread.
  `get_state` and the header agree, and the header shows the level `/effort` reports.
  A composer prompt then runs, and its turn's `model` is the first model in the session file and on the page's turn label

- `picker-auto-kept`

  a pick of `auto`, then a pick of the second model.
  `/effort` still reports `auto`, and the header shows `auto`

- `picker-level-after-model`

  a concrete level of the second model that the first model lacks (`minimal` by default), then a pick of the first model.
  The level the header shows, and the one `/effort` reports, equals what `/effort` reports in a twin session after a person's `/model <first>` Send.
  The twin is a second session of the same profile, brought to the same model and level through RPC

- `picker-effort`

  a pick of a concrete level (`medium`).
  `/effort` reports it, and the header shows it

- `picker-during-turn`

  while a prompt runs `sleep 45` in the bash tool, a pick of the second model.
  The session answers within 30 s, while `get_state` still says the turn is streaming.
  The run's request that was already running is on the first model, and each request started after the answer is on the second.
  The next composer prompt runs on the second model

- `picker-refusal`

  the injected list described above, then a pick of `google/gemini-2.5-flash`.
  The switch line reads `The session answered: Failed to set model: No API key for google/gemini-2.5-flash`.
  `get_state`, `/effort` and the header are unchanged.
  A reload brings back the session's own list

- `forged-switch-changes-nothing`

  two sends to the session.
  First, a frame forged with the listener token, in `dispatch-user-turns.sh`'s forged-frame shape:
  a person-shaped steer naming a message Dispatch never stored, with the body `/model <the other model>`.
  Second, an Envoy message `/effort <another level>` from a second client registered with the listener.
  The session file records the forged attempt as handled (`envoy-dispatch-handled-attempt`), and both arrive as cards.
  `get_state` and `/effort` are unchanged.
  The check prints the values before, the values after, and what each would read had a switch been applied, which is its negative control

- `agent-token-refused`

  `POST /api/v1/agents/<id>/messages` with the body `/model <the other model>` and the session's Dispatch bearer gets 403 `OAUTH_SCOPE_REQUIRED`: that bearer holds no `person` scope.
  Dispatch stores nothing new, and the session is unchanged

- `other-session-unchanged`

  a second session of the same profile, started before every switch, still reports the model and level it started with (`get_state`, `/effort`)

- `no-answer`

  the session's Oh My Pi gets `SIGSTOP`, and then a model is picked.
  Dispatch's send does not fail: its attempt stays `pending` or `sent`.
  After 30 s the switch line reads the no-answer text, the header keeps the values it had, and the pickers come back.
  The process gets `SIGCONT` 65 s after the message was stored, so the late frame arrives as a card and a handled attempt.
  The session's log records Dispatch's refusal (`attempt 1 was requested more than a minute ago`).
  The same accept, asked again with the session's bearer, answers 409 `ACCEPT_STALE`.
  `get_state` is unchanged, and Dispatch records no acceptance

- `resume-keeps-switch`

  one page, held open from before the first relaunch to the last step, and three steps.
  **The switch stays (A2):** the process ends and is started again with `--resume <session file>`, which opens the same session;
  `get_state` and `/effort` show the model and level picked last.
  **Numbering:** once the resumed process has registered, a bare NATS client subscribes to `agentstream.<session>.control` and waits up to 30 s for the first control message the relay sends there (its `watch`, about every 10 s while a page is open), then 1 s more;
  RPC `set_thinking_level` to another level then shows on the held page within 20 s with no reload, and the level set back shows too.
  **Arming:** the process ends again and is started with `--resume <session file> --model <the other model>`;
  `get_state` shows that model, and with no RPC change and no reload the held page shows it within 20 s of the registration.
  The screenshot is the held page then.
  RPC `set_model` (and `set_thinking_level`, when the level moved) puts the session back on the model and level picked last, and the reloaded header shows them.
  A held-page step that times out reloads the page, to tell a report the page missed from one never made, and fails naming its step

- `new-session-defaults`

  a new session of the same profile reports the profile's default model and `low`, through RPC and on its own live view

- `fallback`

  a pick of `rig-broken/broken-model`, then a composer prompt.
  That request fails, and the session file gains a `model_change` with role `fallback` to the first model.
  The header shows the first model within 20 s of that entry, with no further user message.
  The check prints how each request of the run ended, including the retry on the fallback model

- `attribution`

  `bob` sends a switch with `X-Dispatch-User: bob`, the header's own body (`/model <id>` as a steer).
  Dispatch stores it as `bob`'s, the session answers it, and `alice`'s held-open page shows `bob` beside it in the thread

The page's notice for a session that sends no `settings` frame, a pi-envoy older than the frame, has no check here.
This checkout's listener refuses every pi-envoy release before 14.1.0, the first that asks for a session consumer (`consumerPluginMinimumVersion` in `packages/envoy/cmd/listener/session_delivery.go`), and every release from 14.1.0 on sends the frame (`packages/pi-envoy/CHANGELOG.md`), so no session this stack serves can show the notice.
`packages/dispatch/e2e/agent-view-settings.e2e.ts` holds the page to it.

## dispatch-claimant-delivery.sh

```sh
DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
  [DISPATCH_BIN=<envoy-dispatch>] bash scripts/e2e/dispatch-claimant-delivery.sh     # → "dispatch claimant delivery e2e: PASS in <n>s", exit 0
```

Runs on any Linux host with `go`, `bun`, `curl`, `psql`, `openssl` and `python3` (for [`lib/loopback-dsn.py`](#libloopback-dsnpy)) — no Docker.
It proves `addClaimantRoute` (`packages/envoy/internal/dispatch/outbox/publisher.go`) on the wire:
the real `envoy-dispatch` publishes each event, and the run reads which `notifications.agent.*` topics carried its envelope, `dispatch-<event id>`.

Each item below names an input and its default, then what it means.

- `DATABASE_URL` — required

  a scratch PostgreSQL database with `pgvector` available, on `127.0.0.1`, `localhost` or `::1`: [`lib/loopback-dsn.py`](#libloopback-dsnpy) asks libpq which host the DSN names, and the run refuses every other answer.
  The run empties its `public` schema with [`lib/loopback-dsn.sh`](#libloopback-dsnsh)'s `empty_public_schema` before the server migrates it: every table, view, routine and type goes, and the extensions stay.
  It leaves what it wrote there for inspection

- `DISPATCH_BIN` — unset

  a prebuilt `envoy-dispatch` to run; unset, the script builds `./cmd/dispatch` from this checkout's `packages/envoy`

- `CLAIMANT_E2E_DISPATCH_PORT` — `19051`

  the server's port; refused when something already answers on it

- `CLAIMANT_E2E_ENVOY_PORT` — `19053`

  the fake Envoy listener's port; refused the same way

The script drops every inherited `DISPATCH_*`, `ENVOY_*` and `NATS_*` variable (and `PGHOSTADDR`, `PGSERVICE` and `PGSERVICEFILE`) before it starts anything, so a Legion pane's real listener, Dispatch and NATS credentials reach none of it.
It starts a NATS of its own (`internal/testnats/cmd/natsd`, JetStream on, on a port the kernel picks), the harness's fake Envoy listener (`packages/dispatch/e2e/fake-envoy.ts`, where the three sessions are registered live and P and C advertise `steer`, a mention's default mode), and the server with NATS on, dev sign-in, an insecure cookie and an agent token minted for the run, in an environment holding nothing else.
The driver, `dispatch-claimant-delivery.ts`, subscribes to `notifications.>` on that NATS, acts as the sessions P, C and Q with the agent token (each write naming its session as `actor`) and as one person through `GET /auth/_dev/signin`, finds each event's id in the issue's event log, waits up to 10 s for its envelope on the issue's own topic and 2 s more for its other destinations, and prints the agent topics that carried it.
The mention cases listen 15 s rather than 2 s:
while a mention's send is pending the outbox holds back the mentioned session's copy and re-checks the event on its 5 s tick.
Issue `CLM-1` is the person's, with a spec and no route, claimed by P.

Each item below names a case and how it is driven, then what the event's envelope must have reached for the case to pass.

- `case3-ask-answer` — C opens an ask on `CLM-1`; the person answers it

  exactly `agent.C` and `agent.P`.
  It also prints `claimant`, `route` and `route_holder` of `GET /api/v1/asks/{id}` and of the ask's Inbox row's thread (`absent` on a server that predates them)

- `case2-thread-reply` — the person comments on a passage of `CLM-1`'s spec, then replies in that thread, mentioning nobody

  `agent.P`, among any others

- `untargeted-message` — the person posts a message on `CLM-1` with no target

  exactly `agent.P`

- `targeted-message` — the person sends a message on `CLM-1` targeted at `session:Q`

  no `agent.P`

- `agent-comment` — C comments on `CLM-1`

  no `agent.P`

- `unclaimed-issue` — the person comments on `CLM-2`, which nobody claims and which has no route

  no agent topic at all

- `mention-delivered-claimant` — the person comments on `CLM-1` mentioning `session:P`; the fake listener takes the send.
  It prints the payload's mentions (`session:P→P`), the sends the listener took and the comment's delivery rows from `GET /api/v1/comments/{id}`

  no `agent.P` within 15 s (the mention is P's one copy), and the delivery row `sent`

- `mention-failed-claimant` — the same, with the fake listener answering every send to P 404 while its registry read still finds P, so the mention resolves to P and its send fails

  `agent.P` within 15 s, and the delivery row `failed`

- `mention-failed-follower` — C opens an ask on `CLM-3`, which nobody claims, so C only follows it;
  the person replies in the ask mentioning `session:C`, and the fake listener answers every send to C 404.
  It prints the ask's `claimant`, `route` and `route_holder`

  `agent.C` within 15 s, and the delivery row `failed`

Each mention case fails when the comment.created payload's mention does not carry the mentioned session's `session_id`:
the cases are about a mention that resolved, whatever its send did.
A comment answers its mention attempts `pending` and its sender settles them after the response, so each mention case reads the comment's delivery rows once none is `pending`, and fails when one still is 10 s after the 201 (two of the listener client's 5 s windows).

Every case runs and prints `== <case>`, what it read, and `CHECK <case>: PASS` or `CHECK <case>: FAIL: <why>`, as each step before the cases does; a summary follows, and any FAIL exits 1.
The `envoy-session-stall` job of `ci.yaml` runs it on a database of its own on the job's Postgres service.
The work directory, under `TMPDIR`, holds the binaries and the logs of natsd, the fake listener and the server:
removed when the run passes, kept (without the binaries) and printed when it fails.
Every process the run started is stopped on any exit.

**Negative control.**
Built from the pull request's base commit, `81a3b5ef7b47`, which has no claimant route, the same run must fail the three cases the claimant route adds and `mention-failed-claimant`, and pass the rest.
`mention-failed-follower` passes there by design: the base always sent the follower its copy, mentioned or not, and the case proves that behaviour is kept.
A binary built from the pull request's earlier head `15e414ed9b6b`, which skipped a mentioned session's copy as soon as the mention resolved, fails both `mention-failed-claimant` and `mention-failed-follower`.
Neither serves `route_holder`, printed `absent` there:

```sh
mkdir -p "$TMPDIR/base-src"
git --git-dir=<the checkout's git directory> archive <base commit> packages/envoy | tar -x -C "$TMPDIR/base-src"
go -C "$TMPDIR/base-src/packages/envoy" build -o "$TMPDIR/envoy-dispatch-base" ./cmd/dispatch
DATABASE_URL=… DISPATCH_BIN="$TMPDIR/envoy-dispatch-base" bash scripts/e2e/dispatch-claimant-delivery.sh
# → FAIL case3-ask-answer (agent.C alone), FAIL case2-thread-reply, FAIL untargeted-message and
#   FAIL mention-failed-claimant (no agent topic), ok for the other five, exit 1
# Built the same way from 15e414ed9b6b:
# → FAIL mention-failed-claimant and FAIL mention-failed-follower (no agent topic), ok for the
#   other seven, exit 1
```

## verification-results.sh

```sh
DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
  [DISPATCH_BIN=<envoy-dispatch>] [VERIFICATION_RESULTS_E2E_DISPATCH_CLI=<bin/dispatch.ts>] \
  bash scripts/e2e/verification-results.sh     # → "verification results e2e: PASS in <n>s", exit 0
```

Runs on any Linux host with `go`, `bun`, `curl`, `psql`, `openssl` and `python3` (for [`lib/loopback-dsn.py`](#libloopback-dsnpy)) — no Docker.
It proves `comparisonReader` (`packages/envoy/internal/dispatch/api/results_comparison.go`) and `recordPullRequestHead` (`packages/envoy/internal/dispatch/delivery/pull_request_heads.go`) on the wire:
the real `envoy-dispatch` records each result and says where its commit stands against the commits and branches its check compares with, and the head a `before_merge` result is compared with moves only because a pull-request event reached the bus.
Two of its cases prove the rest of that answer:
the bases each check names, read through the `dispatch results` command as well as through the API, and the attribution that decides which issue a pull request's head answers for (`packages/envoy/internal/dispatch/delivery/attribution.go`), the reconcile's re-judging of a stored row included.
Three more cases prove the `dispatch` command itself (`parseCommand` in `packages/envoy-client/src/dispatch-command.ts`).
It refuses a bare `-` given to any text flag its help lists, except `dispatch artifact --path`, whose value names a local file.
It keeps every other value of a text flag byte for byte.

Each item below names an input and its default, then what it means.

- `DATABASE_URL` — required

  a scratch PostgreSQL database with `pgvector` available, on `127.0.0.1`, `localhost` or `::1`: [`lib/loopback-dsn.py`](#libloopback-dsnpy) asks libpq which host the DSN names, and the run refuses every other answer.
  The run empties its `public` schema with [`lib/loopback-dsn.sh`](#libloopback-dsnsh)'s `empty_public_schema` before the server migrates it: every table, view, routine and type goes, and the extensions stay.
  The `envoy-session-stall` job gives it a database of its own on that job's PostgreSQL, the pgvector image.
  The run leaves what it wrote there for inspection.
  The driver reads it too, as `VERIFICATION_RESULTS_E2E_DATABASE_URL`, for the one fixture no route of Dispatch can write: a stored pull request row of the attribution rule before this build's, since every write of this build stamps the rule it judges by

- `DISPATCH_BIN` — unset

  a prebuilt `envoy-dispatch` to run; unset, the script builds `./cmd/dispatch` from this checkout's `packages/envoy`

- `VERIFICATION_RESULTS_E2E_DISPATCH_CLI` — unset

  the `bin/dispatch.ts` the driver runs in place of this checkout's `packages/envoy-client/bin/dispatch.ts`.
  The script reads it before it drops the inherited variables, and refuses a value that is not a readable file.
  The script makes the path absolute, because `dash-refused` runs the command in another directory.
  A negative control names the command of another tree here, with no change to the checkout

- `VERIFICATION_RESULTS_E2E_DISPATCH_PORT` — `19061`

  the server's port; refused when something already answers on it

- `VERIFICATION_RESULTS_E2E_ENVOY_PORT` — `19063`

  the fake Envoy listener's port; refused the same way

- `VERIFICATION_RESULTS_E2E_GITHUB_PORT` — `19065`

  the fake GitHub's port; refused the same way, and the three must differ

The script drops every inherited `DISPATCH_*`, `ENVOY_*`, `NATS_*` and `FAKE_*` variable (and `PGHOSTADDR`, `PGSERVICE` and `PGSERVICEFILE`) before it starts anything, so a Legion pane's real listener, Dispatch and NATS credentials reach none of it.
It starts a NATS of its own (`internal/testnats/cmd/natsd`, JetStream on, on a port the kernel picks), the harness's fake Envoy listener (`packages/dispatch/e2e/fake-envoy.ts`), the harness's fake GitHub (`packages/dispatch/e2e/fake-github.ts`, which the server reads as `DISPATCH_GITHUB_API_BASE`), and the server with NATS on, dev sign-in, an insecure cookie, an agent token minted for the run and a throwaway RSA App key, in an environment holding nothing else.
The App key is what makes the delivery intake run at all, and every GitHub call it makes goes to the fake.
The server also runs its delivery reconcile every two seconds, and its first pass as it starts (`DISPATCH_TEST_HOOKS=1`, `DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL=2s`, `DISPATCH_TEST_RECONCILE_AT_START=1`), in place of the five minutes a deployment runs it at:
the attribution case waits for one pass to re-judge two stored rows.

The driver, `verification-results.ts`, acts as one session with the agent token (each write naming the session as `actor`) and as the person `alice` through `GET /auth/_dev/signin`.
It also runs the `dispatch` command as a process: the `bin/dispatch.ts` that `VERIFICATION_RESULTS_E2E_DISPATCH_CLI` names, else this checkout's `packages/envoy-client/bin/dispatch.ts`.
The setup line names the command the run uses.
Each run gets `DISPATCH_HOST=omp`, the run's own session, `DISPATCH_URL` and token, and a scratch `HOME`, and the driver gives it a stdin and a working directory when a step needs them.
The bases case reads two of its answers through the command, which is the surface a reviewer reads.
The three cases of a bare `-` drive the command for every step, and judge each step by its exit code and what it printed.
The driver's setup seeds the fake GitHub with two repositories — `acme/verification-live`, whose default branch `main` holds three of the run's commits, and `acme/verification-hidden`, which the installation lookup answers 404 for — maps each as a project's default repository, and writes the one `delivery_settings` row the intake needs before it binds its durable.
`acme/verification-live`'s `commits` names every commit a case records at, since the record route asks GitHub whether the repository holds a result's commit and refuses one it does not; the one commit it names nowhere is what the refusal case drives.
`commit_pulls` is empty on purpose:
the record route's read-through asks GitHub which pull requests name a result's commit, so with no answer there the only thing that can teach Dispatch a head is the envelope a case publishes.
`pulls`, what GitHub answers about one pull request, is empty until the attribution case seeds the two its own stored rows name, since one reader alone reads it: the reconcile's refresh of a stored head row.
Each case after the first seeds the issue it drives, with a spec that marks one requirement and a Verification document holding that case's checks.

Each item below names a case and how it is driven, then what must hold for it to pass.

- `the-results-route-is-served` — a result naming a check a plain issue does not have

  `400 UNKNOWN_CHECK`, which is the run's first result write.
  A binary that serves no such route answers `404`, and the run stops there, since every later case records a result too

- `a2-record-and-read` — one result recorded against a `before_merge` check, then read back through `GET /api/v1/issues/{key}/results`

  exactly one result under that check, at the commit it was recorded at, with the verdict and the `observed` it carried, and the pull-request body line Dispatch printed for it

- `a4-a-push-moves-the-head` — the pull request's `opened` envelope on `notifications.github.acme.verification-live.pr.7`, a result at that head, then its `synchronize` envelope at a later head, and a result at the new one

  the first result at that head once the envelope has landed, a pass of the check standing from the tester, then the same result at none of the compared commits after the push and no pass standing — with no write of Dispatch's, the reads alone — and the result at the new head standing there again

- `a-fork-pull-request-is-no-basis` — the same pull request published twice, first with its head branch in `stranger/verification-live` and then with its head in `acme/verification-live`, with one result at that head

  the check comparing with nothing while the head is the fork's, for `no_pull_request_or_branch`, and the result standing at that head once the same pull request names a head of this repository

- `a5-a-closed-issue-takes-the-after-merge-result` — the issue closed, then a result for its `after_merge` check, a result for its `before_merge` check, and a document write

  the after-merge result recorded, and `409 ISSUE_CLOSED` for both the before-merge result and the document write

- `a10-a-default-branch-result-follows-the-branch` — one `default_branch` check with a result at a commit the fake's compare calls `behind` and another at a commit it calls `diverged`

  the first at the branch the check compares with, the second at another commit

- `a-result-at-a-commit-the-repository-does-not-hold-is-refused` — a result at a 40-hex commit the fake GitHub was seeded with nowhere, against a `before_merge` check of an issue that resolves to `acme/verification-live`

  `400 RESULT_COMMIT_UNKNOWN`, naming the whole commit and `acme/verification-live`, and no result under that check afterwards

- `an-unreadable-repository-answers-unknown` — a `default_branch` result and a `before_merge` result on an issue whose project maps to `acme/verification-hidden`

  both unknown, naming the unreadable repository, and whether a pass of either check stands unknown with them:
  Dispatch can reach no installation token for that repository, so it can say nothing about where either commit stands

- `k-compared-commit-the-bases-each-check-names` — one issue holding a `before_merge`, an `after_merge` and a `default_branch` check, with two open pull requests at the heads A and A2, a merged pull request whose base branch `release/old` the fake holds no commits for, and five results: one at A, one at A2, one at a commit that heads no pull request, and one for each branch check at a commit `main` carries.
  The run reads them through `dispatch results` with no `--at`, with `--at A` and with `--at B` (a commit no result is at), and through `GET /api/v1/issues/{key}/results` and `?at=`, then publishes the `synchronize` envelope that moves A to A3 and reads the first check again.
  Its last read is of an issue whose project maps to `acme/verification-hidden`

  the before-merge check naming the head of every pull request of the issue, open and merged, each with its number and the time Dispatch learned it.
  Each result stands at its own pull request's head, which is this case's control:
  a read that named one head alone would leave the result at A2 at another commit and fail here.
  The after-merge check names `release/old` and `main`, and its result stands at `main`, the branch that carries it, named;
  the default-branch check names `main` alone.
  With a commit named, to the command or to the API, every check of the issue compares with that commit and nothing else, the result at A2 reads at another commit, and a named commit no result is at leaves every result there.
  After the event the basis names A3 at the event's own update time, as the pull request's update time and not as a read.
  The unreadable repository is reported as that, with no pass standing, and never as nothing to compare with

- `k-attribution-what-decides-a-pull-request-issue` — five pull requests of one parent and its child: one on `legion/<child>` whose title and body name the parent first and the child second, one whose branch names no issue and whose body names one, one on `legion/<child>` whose URL a person linked to a third issue, one whose branch names no issue, whose title names one issue and whose URL a person linked to another, and two rows the driver stores credited to the parent under attribution rule 1, one open and one merged, which only the reconcile can re-judge

  the branch deciding the first, the body the second, and the person's link the third and the fourth, each row naming the source that decided it (`branch`, `title_body`, `external_link`).
  The child's result stands at its pull request's head, while the parent's check takes none of it and its result at that commit reads at another commit.
  The reconcile moves both stored rows from the parent to the child, the merged one included, and each move appends one `issue.pull_request_attribution_changed` on the parent and one on the child, naming the issue the pull request left, the issue it reached and the source.
  The controls are in the case:
  the second pull request is on a branch that names no issue, so the branch rule swallows no other source;
  the third is on the child's own branch, so the person's link stands above it;
  and the last envelope moves a head without moving its attribution, which appends no event, so an event marks a change and never a pass

- `dash-refused` — one issue with the `before_merge` check `k1`, and eight calls of the command, in order.
  Step 1 is the reported call, `dispatch result --issue <issue> --check k1 --role implementer --verdict pass --commit <commit> --run - --observed - --negative-control -`, with a here-document on stdin.
  Step 2 gives `--run=-` alone, and step 3 gives `--observed -` with `--dry-run`.
  Step 4 is `dispatch comment --issue <issue> --body -`, and step 5 is `dispatch message --issue <issue> --body -`.
  Step 6 is `dispatch issue --project VRF --title -`.
  Step 7 is the call the refusal of step 1 leads to: `--run-file <file A>`, `--observed-file -` with the here-document, and `--negative-control-file <file B>`.
  Step 8 is `dispatch artifact --issue <issue> --name dash.txt --path -`, in a directory that holds a file named `-`

  step 1 exits 2, prints exactly the first text of R1 of dispatch://INFRA-2618/spec and a line feed, and prints nothing on stderr.
  When step 1 exits 0, the failure names the `run`, `observed` and `negative_control` that `k1` then stores.
  Steps 2 to 5 each exit 2.
  Between the `was not called` line and the `- Example:` line, each prints exactly the one problem line for the flag it gives `-`, and none prints a `- Allowed flags:` line.
  Step 6 exits 2 and prints exactly the second text of that requirement and a line feed.
  The project holds as many issues after step 6 as before it, each count read over every page of `GET /api/v1/issues?project=VRF`.
  After step 6, the issue holds no result, no comment and no message.
  Step 7 exits 0, and `k1` then holds one result, whose `run`, `observed` and `negative_control` equal the bytes of file A, the here-document and file B.
  Step 8 exits 0, and version 1 of `dash.txt`, read through `GET /api/v1/issues/{key}/artifacts/dash-txt/versions/1`, holds the bytes of the file named `-`

- `dash-refused-every-flag` — `dispatch --help`, then `dispatch <command> --help` for each command it lists.
  For each `--X-file <path>` row whose `--X` the help lists with a value other than `<json>`, the case calls `dispatch <command> --X - --dry-run`.
  It leaves out `--path-file` of `dispatch artifact`, because `--path` names a local file to upload

  each call exits 2, and its output holds, as a whole line, the problem line that R1 gives for `--X`.
  The case reads the line as well as the exit code, because a dry run that lacks a required flag also exits 2.
  The case stops at the first call that fails, and names its command, its flag, its exit code and its output.
  It prints each command with the flags it called, then the two counts.
  It fails when it called fewer than 28 commands or fewer than 112 flags

- `values-kept` — one issue with three `before_merge` checks, and five steps.
  Step 1 records a result for the first check with `--run "go test ./internal/workflow/ -run 'AppendOnly|Line|StartWrittenOverAnOwedRow' -count=1"`, with `--observed-file -` fed the two lines `--- PASS: TestAppendOnly (0.00s)` and `ok`, and with `--negative-control "-run 'A|B' with the guard taken out: 2 tests fail"`.
  Step 2 records a result for the second check with `--observed='--- PASS: TestLine (0.00s)'`, and with `--negative-control-file` naming a file that holds the one character `-` and no line feed.
  Step 3 records a result for the third check with `--observed-file -` fed the one character `-` and no line feed.
  Step 4 gives the third check `--observed '--- PASS: TestLine (0.00s)'`, the value as a word of its own.
  Step 5 reads `GET /api/v1/issues/{key}/results`

  steps 1 to 3 each exit 0.
  Step 4 exits 2 and prints the whole line `- --observed needs a value`, as the command did before this change.
  In step 5 each check holds one result.
  Each stored `run`, `observed` and `negative_control` equals byte for byte what its step gave, and the stdin text of step 1 keeps both its line feeds.
  A field that a step did not send reads as the empty string

Every case prints `== <case>`, what it read, and `CHECK <case>: PASS` or `CHECK <case>: FAIL: <why>`, as each step before the cases does; a summary follows, and any FAIL exits 1.
The `envoy-session-stall` job of `ci.yaml` runs it on a database of its own on the job's Postgres service.
The work directory, under `TMPDIR`, holds the binaries and the logs of natsd, the two fakes and the server:
removed when the run passes, kept (without the binaries) and printed when it fails.
Every process the run started is stopped on any exit.

**Negative control.**
Built from the pull request's base commit, `cd99d1b266d7`, which serves no `POST /api/v1/issues/{key}/results`, the same run fails at `the-results-route-is-served` and stops there:

```sh
mkdir -p "$TMPDIR/base-src"
git --git-dir=<the checkout's git directory> archive <base commit> packages/envoy | tar -x -C "$TMPDIR/base-src"
go -C "$TMPDIR/base-src/packages/envoy" build -o "$TMPDIR/envoy-dispatch-base" ./cmd/dispatch
DATABASE_URL=… DISPATCH_BIN="$TMPDIR/envoy-dispatch-base" bash scripts/e2e/verification-results.sh
# → FAIL the-results-route-is-served: the record route answers 404 NOT_FOUND, no other case run,
#   exit 1
```

The three cases of a bare `-` take their negative controls from `VERIFICATION_RESULTS_E2E_DISPATCH_CLI`, so the checkout does not change.
For `dash-refused` and `dash-refused-every-flag`, the input names the command of a git worktree of the commit this change starts from:

```sh
git -C <checkout> worktree add --detach "$TMPDIR/<base tree>" <base commit>
(cd "$TMPDIR/<base tree>" && bun install --frozen-lockfile)
DATABASE_URL=… [DISPATCH_BIN=<envoy-dispatch>] \
  VERIFICATION_RESULTS_E2E_DISPATCH_CLI="$TMPDIR/<base tree>/packages/envoy-client/bin/dispatch.ts" \
  bash scripts/e2e/verification-results.sh
# → FAIL dash-refused: step 1: dispatch result … --run - --observed - --negative-control - exited 0,
#   want 2: k1 stores run "-", observed "-", negative_control "-"
# → FAIL dash-refused-every-flag: step <n>: dispatch <command> <flag> - --dry-run exited <code>,
#   want 2: its output holds no problem line for <flag>
#   exit 1
git -C <checkout> worktree remove --force "$TMPDIR/<base tree>"
```

For `values-kept`, the input names the command of a git worktree of this change's head, where the new rule refuses every value that starts with `-` and not the value `-` alone:

```sh
git -C <checkout> worktree add --detach "$TMPDIR/<head tree>" <head commit>
(cd "$TMPDIR/<head tree>" && bun install --frozen-lockfile)
# In "$TMPDIR/<head tree>/packages/envoy-client/src/dispatch-command.ts", the bare-`-` rule of
# parseCommand tests `value === STDIN`: change that test to `value.startsWith(STDIN)`.
DATABASE_URL=… [DISPATCH_BIN=<envoy-dispatch>] \
  VERIFICATION_RESULTS_E2E_DISPATCH_CLI="$TMPDIR/<head tree>/packages/envoy-client/bin/dispatch.ts" \
  bash scripts/e2e/verification-results.sh
# → FAIL values-kept: step 1: dispatch result … exited 2, want 0, and its stdout holds the problem
#   line for --negative-control
#   exit 1
git -C <checkout> worktree remove --force "$TMPDIR/<head tree>"
```

## step-records-read.sh

```sh
scripts/e2e/step-records-read.sh <ISSUE>     # → "PASS: <ISSUE>: <n> records; …", exit 0
```

The check after the merge: once the release that records each step reaches a deployment, one real issue runs to done there, and this script reads that issue's step records.
It runs `dispatch steps --issue <ISSUE>` once and prints what the command printed, then one verdict.
It runs where the operator runs `dispatch` by hand, with the operator's own credential: the `dispatch` command on PATH, the session the operator's shell names (`DISPATCH_HOST`, `DISPATCH_SESSION_ID`) and the deployed Dispatch and token the operator's `envoy.json`, or `DISPATCH_URL` and `DISPATCH_TOKEN`, name.
It sets none of those, reads no credential of its own, writes nothing and starts nothing.
It reads an issue that is done, so every step of it has ended.

| exit | printed | when |
| :--- | :--- | :--- |
| 0 | `PASS: <ISSUE>: <n> records; …` | the read lists a record of each of steps 1 to 5, of the retro and of the merge; every record has ended; every record of kind `step` or `retro` names an agent; every record of kind `step` names the model that answered; a merge record with no agent passes with the line `the merge has no agent: no merger ran it: <line>`, since GitHub merged the pull request before a merger started and the record says so |
| 1 | one `FAIL: …` line per fault | the read lists records and one does not hold: a place with no record (`no record of the merge`), a record that never ended, a step or retro with no agent, a step with no model; each line names the record as the command printed it |
| 2 | `UNKNOWN: <reason>` | nothing to judge: the read lists no record of the issue (`dispatch steps lists no record of <ISSUE>`), the command failed (its exit and what it printed), no `dispatch` is on PATH, or the answer is no read of the issue (no header, a line that is no run, a header whose count differs from the lines below it); a usage error is exit 2 too, with the usage on stderr |

`scripts/e2e/lib/step-records-read.test.ts` (`bun test scripts/e2e/lib`, in CI's `test` job) runs it against a `dispatch` stand-in on PATH that prints a recorded answer: a whole answer passes, the merge record missing fails, an empty read is `UNKNOWN`, and each fault above has its row.

## plan-ratings-read.sh

```sh
scripts/e2e/plan-ratings-read.sh <PROJECT> <ISSUE>     # → "PASS: <ISSUE>: plan …, ratings …, time …, failed steps <n>", exit 0
```

The check after the merge: once the release that rates each plan reaches a deployment, one real issue of the project runs to done there, and this script reads it among the project's finished issues.
It runs `dispatch plans --project <PROJECT>` once and prints what the command printed, then one verdict on the line of `<ISSUE>`, an issue of `<PROJECT>`.
It runs where the operator runs `dispatch`, with the operator's own credential: the `dispatch` command on PATH, the session the operator's shell names (`DISPATCH_HOST`, `DISPATCH_SESSION_ID`) and the deployed Dispatch and token the operator's `envoy.json`, or `DISPATCH_URL` and `DISPATCH_TOKEN`, name.
It sets none of those, reads no credential of its own, writes nothing and starts nothing.
It reads no file but `lib/dispatch-read.sh` beside it, so a copy of the two runs outside a checkout of this repository too.
It judges no other issue: a finished issue Legion never ran lists `plan none`, `ratings none` and `time -`, and one it ran before the release that rates each plan lists `plan none` and `ratings none`, with the span of the step records the release that records each step wrote of it, or `time -` where there are none, which is true of what Dispatch holds of each.
The issue's own `time -` is no fault either: it is Dispatch knowing no span of the issue's step records (no record has a start, none has an end, or the latest end is before the earliest start), which says nothing of its plan or its ratings, so the verdict prints that time as `unknown` and judges the rest.

| exit | printed | when |
| :--- | :--- | :--- |
| 0 | `PASS: <ISSUE>: plan <plan>, ratings <ratings>, time <time>, failed steps <n>` | the read lists the issue with a plan and at least one rating, each rating with a reason; the line names each rating with its reason as the command printed them, and the time as printed, or `unknown` for `-` |
| 1 | one `FAIL: …` line per fault | the read lists finished issues and the issue's line does not hold: no plan, no rating, a rating with an empty reason, each such line naming the issue's line as the command printed it; or the read lists no line of the issue (`the read lists <n> finished issues of <PROJECT> and no line of <ISSUE>`), or lists it twice (`the read lists <ISSUE> <n> times`) |
| 2 | `UNKNOWN: <reason>` | nothing to judge: the read lists no finished issue of the project (`dispatch plans lists no finished issue of <PROJECT>`), the command failed (its exit and what it printed), no `dispatch` is on PATH, or the answer is no read of the project (no header, a line that is no finished issue, a rating that is not `<n>/<top> "<reason>"`, a header whose count differs from the lines below it); a usage error is exit 2 too, with the usage on stderr: a project key and the key of an issue of that project |

`scripts/e2e/lib/plan-ratings-read.test.ts` (`bun test scripts/e2e/lib`, in CI's `test` job) runs it against a `dispatch` stand-in on PATH that prints an answer the real command rendered: envoy-client's `dispatch plans` code prints typed finished issues, so a line it prints that this script cannot read fails the test.
Among its cases: a whole answer passes, an issue rated twice passes, an issue its owner planned passes, an empty read is `UNKNOWN`, and each fault above has its row.
The `daemon-live` job's `TestPlanRatingsLive` (`internal/daemon/plan_ratings_live_test.go`) runs it against the Dispatch that test builds, with a `dispatch` on PATH that runs the same CLI, so it also reads finished issues a real Dispatch served rather than ones the test typed.

## envoy-session-delivery.sh

The test of session delivery: every Envoy event reaches a real Oh My Pi session and a real Claude Code session exactly once, also when it was published while the session, its NATS connection, either listener or Dispatch was down, and when it was stored for a session before that session first registered.
The agent-harness job's `session-scripts` part runs it as a step.

```bash
LEGION_TEST_OMP=<omp> LEGION_TEST_CLAUDE=<claude> LEGION_TEST_PG_DSN='postgres://postgres@127.0.0.1:<port>/postgres?sslmode=disable' \
  bash scripts/e2e/envoy-session-delivery.sh     # → "ENVOY SESSION DELIVERY: PASS …", exit 0

# Negative controls: the listener and both plugins of an earlier revision.
… bash scripts/e2e/envoy-session-delivery.sh --build-from c3d09eec --expect-lost agent,issue,role
… bash scripts/e2e/envoy-session-delivery.sh --build-from 926040d9048572765639e56d516226b822288b46 --expect-lost role

# The rollout state production meets first: this checkout's hub with the plugins of the release
# before it.
… bash scripts/e2e/envoy-session-delivery.sh --plugins-from origin/main

# The upgrade: the sessions start on the plugins of <rev> and are resumed on this checkout's, so
# one session's own life crosses the release — the move every Legion worker makes when its image
# is re-pinned. Only with disruption a, which is the window it upgrades in.
… bash scripts/e2e/envoy-session-delivery.sh --disruptions a --resume-plugins-from origin/main
```

Both sessions' model is [`lib/session-delivery-model.ts`](lib/session-delivery-model.ts), a stand-in on [`lib/messages-standin.ts`](lib/messages-standin.ts) that answers the setup prompt with its two Envoy tool calls (through Claude Code's ToolSearch where Claude Code defers them) and every other turn `ok`.
The sessions reach it through `ANTHROPIC_BASE_URL`, keyed by a placeholder of the run's own as their `ANTHROPIC_API_KEY`, and it refuses a request without that key.
No caller's `ANTHROPIC_*` variable reaches anything the run starts.

It needs no Docker, tmux or root.
It needs `go`, `bun`, `jq`, `curl`, `socat` 1.8.0 or later (its `ACCEPT-FD` address), `setsid`, `psql`, `git`, `tar`, `pgrep`, `mkfifo` and `python3` (for [`lib/held-port.py`](#libheld-portpy)), and the checkout's `node_modules` (`bun install`).
Everything is built from the checkout:

- `natsd` (`packages/envoy/internal/testnats/cmd/natsd`) with JetStream on a store directory of the run's own;

- two listeners, machine id `shared`, where both sessions register, and machine id `dispatch`, the one Dispatch sends through;

- Dispatch (`envoy-dispatch`), on a database the run creates in the Postgres `LEGION_TEST_PG_DSN` names and drops on exit.
  The script starts no Postgres, and Dispatch needs pgvector;

- an Oh My Pi session, `omp --mode rpc --no-extensions -e <pi-envoy>`, the plugin packed by [`lib/pack-plugin.sh`](#libpack-pluginsh);

- a Claude Code session, `legion claude-bridge -- claude -p --input-format stream-json
  --output-format stream-json --verbose --replay-user-messages --thinking-display summarized …`,
  configured as `packages/daemon/internal/runtime/tmux/claude.go` configures a Legion pane:
  an MCP config that runs claude-envoy's `dist/envoy-channel.js`, both plugin directories, `LEGION_CLAUDE_INBOX` from the bridge.
  The claude-envoy plugin is the run's own copy, built from this checkout into its work directory (`packages/claude-envoy/scripts/build.ts --out`), since a pull request's tree carries no fresh `dist/` or `legion/`.
  Claude Code is the binary `LEGION_TEST_CLAUDE` names.

Each session reads an `envoy.json` naming listener `shared` and, as its only NATS server, a `socat` proxy of its own, which the script can cut, with Dispatch off.
Dispatch reaches NATS through a proxy of its own too.
Each session gets one setup prompt, which makes it follow `notifications.dispatch.issue.<KEY>.>` and the run's own team topic `notifications.team.live-delivery-<run>`, and hold a role of its own;
the listener's registry is then read to show them, and `GET /v1/sessions` to show each session's `delivery` reading `consumer` — read again with the table, and a failure either time
— so every count below is a count of what a session's own consumer carried.
Then eight windows run, each with one disruption, and every event of the window is published while the disruption holds:

| window | disruption |
| :--- | :--- |
| a | both sessions SIGKILLed (their whole process groups) and resumed under the same id: `omp --resume <session file>`; the bridge again, with `claude --resume <id>` |
| b | both proxies cut, until natsd's `/connz` shows neither session's agent or relay subscription, then restored |
| c | listener `shared` SIGKILLed and restarted |
| d | listener `dispatch` SIGKILLed and restarted |
| e | Dispatch's own NATS connection cut at its proxy, so its outbox cannot publish what the writes commit; Dispatch's writes made and Dispatch SIGKILLed right after they return; the proxy restored and Dispatch restarted, so events committed before the kill are published after the restart; the run prints the stream's stored time of each comment of the window beside the restart time |
| f | none (the control) |
| g | the Oh My Pi session stopped as a clean shutdown stops it, its stdin closed with no signal, so its plugin deregisters and the listener deletes its session consumer, and then resumed under the same id; the Claude Code session stays up |
| h | the Claude Code session SIGKILLed and replaced by one whose id has never registered, with the window's one envelope published on that id's own agent subject, and stored, before it starts |

Every server takes its port from [`lib/held-port.py`](#libheld-portpy), never a port picked free a moment earlier.
natsd binds the client and monitor ports the kernel chooses (`-p -1 -m -1`), and the script reads both from its log.
Every other server restarts in a window, and a configuration names its port before it first starts:
the sessions' `envoy.json` names listener `shared` and a proxy, and Dispatch's environment names listener `dispatch` and its proxy.
So each serves on a copy of a socket a keeper (`held-port.py keep`, one per server, its socket under `ports/`) holds for the whole run:
each listener through `ENVOY_LISTEN_FD`, Dispatch through `DISPATCH_LISTEN_FD`, and each proxy as `socat ACCEPT-FD:3,fork` (the helper hands the socket over as descriptor 3).
A restart serves the same port, and nothing else can take it while the server is down.
A kept port with no server still listens: the kernel queues a connection and nothing answers it.
A client of a server a window has stopped therefore waits rather than being refused, and the script polls every kept port with a timeout (`curl --max-time`, and for a proxy a read of natsd's `INFO` line within 2 s).
Only a listener of a `--build-from` revision that takes no `ENVOY_LISTEN_FD` (`c3d09eec` and `926040d9` take none) binds a number:
it can bind only the `PORT` it is given, so it gets a port picked free below the kernel's ephemeral range, and its restart binds the same number again.

Windows a to g publish these events.
Each event carries a marker of its own in its text:

| kind | events |
| :--- | :--- |
| agent | `POST /v1/messages/send` to each session; a Dispatch message to each session (an aside from a session actor, on a second issue the sessions do not follow) |
| issue | two Dispatch comments on the followed issue |
| role | `POST /v1/messages/publish` to each session's role; a core NATS publish of an Envoy envelope on `notifications.role.<role>`, which the role lane forwards; each is expected once in every window, since a holder that is not live (window g's cleanly stopped session, whose claim stands) gets a `202` kept copy that its registration hands over when it comes back, and only a `404` publish, which kept nothing, is expected 0 times |
| team | `POST /v1/messages/publish` to the team topic both sessions follow, one event counted in both of them; `notifications.team.>` is a stream subject, so the publish is stored and each session's own consumer holds it until that session is back: in window a, which SIGKILLs both sessions, the row must still read 1 for each |

Window h publishes none of them.
Its one event is a core NATS publish of an Envoy envelope on `notifications.agent.<the new Claude Code session id>`, counted in that session alone.
It is a direct core publish rather than a `POST /v1/messages/send` because the listener refuses a send to a session it does not know, and that nothing knew the session yet is the whole case:
the stream captures `notifications.agent.>`, so the envelope is stored while no session of that id exists, and the listener must give the session's consumer the first sequence the stream holds on its own agent subject when it first registers.
An agent subject is one session's alone, so that start is never older than the session itself.
This is what sjawhar/legion#2720's deleted replay sweep did for a brand-new session.

The listener sends go to listener `dispatch`, and to listener `shared` in window d.
In window d, Dispatch cannot reach its listener:
its lookup of the session (`GET /v1/registry/<id>`) gets no answer within its five-second window on the kept port (a picked port refuses the connection), so it records its two messages as failed attempts with no envelope.
Nothing was published for them, and they are expected 0 times (`/0` in the table).

After each window, the script waits up to `SESSION_DELIVERY_SETTLE_S` for every marker to reach its session, then `SESSION_DELIVERY_HOLD_S` more for a repeat.
At the end it counts each marker in every transcript the session the event was meant for has had.
A session the run replaces writes a new transcript — window h replaces the Claude Code session — so the windows before the replacement are counted in the transcript it had then, and the run is judged as a whole.
Each later transcript's lines are counted on from the one before it, which the session wrote earlier, so the issue-order comparison reads one rising line number per session.
In the Oh My Pi session file it counts `envoy-message` cards and user messages.
In the Claude Code transcript `<CLAUDE_CONFIG_DIR>/projects/*/<id>.jsonl` it counts user messages (no tool results, no meta lines) and the `queued_command` attachments of prompts Claude Code took during a turn.
It does not count the assistant's replies, Claude Code's `queue-operation` and `api-request-blob` lines, or the plugins' own ledger entries.
The script prints one row per event, with its marker and one count per session.
It then reads the followed issue's comments back from the stream and checks that both transcripts show them in the order the stream stored them, their publish order.
Dispatch's outbox decides that order.
When it differs from the order the comments were written in, the run prints a `note:` line and does not judge it:
after a Dispatch process dies holding an outbox claim, the claimed event waits out the outbox's one-minute claim window while later events of the issue are published (`packages/envoy/internal/dispatch/outbox/publisher.go`, `claimWindow`), and window e shows this.
Each step prints `CHECK <step>: PASS` or `CHECK <step>: FAIL: <why>`: prerequisites, build, services, sessions, consumer-delivery, then one line per window from the table (`window-<letter>-<disruption>`), `issue-order` and `sessions-stop`.
The run exits 0 only when every count is the expected one and the order holds.
For each wrong count, `wrong-counts.txt` lists every transcript line that carries the marker.

`--build-from <rev>` builds the listener, pi-envoy (packed by that revision's own `lib/pack-plugin.sh`), claude-envoy and the `legion` bridge from `<rev>`, extracted with `git archive <rev> | tar -x` into the run's directory, its `bun install`, pack and claude-envoy build run with the `bun` on `PATH` (`c3d09eec` and `926040d9` pin Bun 1.3.14 and both install and pack under 1.4.4).
claude-envoy is that tree's `scripts/build.ts --out` build when its `build.ts` takes `--out`;
a revision whose `build.ts` does not (`c3d09eec` and `926040d9` among them) is from before pull requests stopped committing `dist/`, and its committed `dist/` and `legion/` are used.
natsd and Dispatch stay this checkout's.
The bridge's argv carries `--replay-user-messages` only when that revision's `claudelaunch.go` (or, before it existed, `runtime/tmux/claude.go`) does.
`--expect-lost <kinds>` (`agent`, `issue`, `role`, `team`) turns the run into a negative control:
it exits 0 only when every event of those kinds in window a counted 0 in both sessions.
It prints the rest of the table and does not judge it.
Main before sjawhar/legion#2228 (`c3d09eec`) loses agent, issue and role events under a session restart.
The merge-forward head of #2228 (`926040d9`) recovers agent and issue events through `POST /v1/replay` and still loses role forwards.

`--plugins-from <rev>` builds pi-envoy and claude-envoy from `<rev>`, as `--build-from` builds them, and leaves the listeners, natsd, Dispatch and the bridge this checkout's:
the mixed rollout, a hub already updated serving a session that still runs the plugins of the release before it.
`--build-from` and `--plugins-from` each name the tree the plugins come from, so the run takes one or the other.
The listener this checkout builds refuses a registration it can give no session consumer before it writes anything, by the reason the canonical list holds (`ErrorWireSchema.reason`, `packages/envoy-client/src/transport.ts`), so `--plugins-from` names a tree whose plugins ask for one;
a tree from before they did is run with `--build-from`, which serves it the listener of its own revision.

`--disruptions a,c` runs only the windows named, any subset of `a` to `h`.
A subset that publishes no comment of the followed issue (`h` alone) has no comment order to compare, and the run says so rather than judging it.
It says so only on the evidence of its own `events.jsonl`, read back and holding no issue event: a record the run cannot read is named and fails the run, since it tells nothing about what was published.

| input | default | meaning |
| :--- | :--- | :--- |
| `LEGION_TEST_PG_DSN` | required | a loopback Postgres with pgvector; the run creates and force-drops a database of its own there with `psql`, gives Dispatch the connection string [`lib/loopback-dsn.sh`](#libloopback-dsnsh)'s `run_database_url` rebuilds for that database, and refuses the DSN in `setup`, before any build or service, when [`lib/loopback-dsn.py`](#libloopback-dsnpy) refuses the host or the rebuild |
| `LEGION_TEST_OMP` | required | the Oh My Pi the pi session runs |
| `LEGION_TEST_CLAUDE` | required | the Claude Code the cc session runs |
| `SESSION_DELIVERY_MODEL` | `claude-haiku-4-5` | both sessions' model name (Oh My Pi gets `anthropic/<model>`), which the stand-in answers whatever it is |
| `SESSION_DELIVERY_HEARTBEAT_MS` | `10000` | the sessions' `ENVOY_HEARTBEAT_MS` |
| `SESSION_DELIVERY_SETTLE_S`, `SESSION_DELIVERY_HOLD_S` | `60`, `15` | the per-window waits above; window e waits 60 s longer, the outbox's claim window |
| `SESSION_DELIVERY_EVIDENCE_DIR` | a fresh directory under `TMPDIR` | kept and printed: `table.txt`, `events.jsonl` (each event's marker, window, kind, the publisher's answer and the expected count), `logs/` (the builds, natsd, both listeners, Dispatch, the proxies, the stand-in and its requests, and each port's keeper, `keep-<server>.log`), `ports/` (the keepers' port files and sockets) and `sessions/<pi\|cc>/` (each launch's RPC output and stderr, and the transcripts) |

No `DISPATCH_*`, `ENVOY_*`, `NATS_*`, `OMP*`, `PI_*`, `LEGION_*` (but the three `LEGION_TEST_*` inputs), `CLAUDE_*`, `ANTHROPIC_*`, `PGHOSTADDR`, `PGSERVICE` or `PGSERVICEFILE` variable of the caller reaches anything the run starts.
On any exit the run stops everything it started by the pid it was started as (a session or proxy by its process group, and each keeper after the servers) and drops its database.
Exit status: 0 pass, or the negative control holds;
1 a check failed, a missing input or tool among them.
Do not edit the script while a run is going: bash reads it as it runs.

## nats-callout-sessions.sh

```sh
LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
  bash scripts/e2e/nats-callout-sessions.sh [--binaries <dir>]     # → "nats-callout-sessions: 19 of 19 checks passed (hosts: plugins)", exit 0
LEGION_TEST_PG_DSN=… LEGION_TEST_OMP=… LEGION_TEST_CLAUDE=… LEGION_TEST_CODEX=… \
  bash scripts/e2e/nats-callout-sessions.sh --real-hosts [--binaries <dir>]     # → "nats-callout-sessions: 17 of 17 checks passed (hosts: real)", exit 0
```

The live proof of the NATS auth service's `r-kinds` and `r-refusal` with sessions: its verification's check `c-callout-kinds-plugins` in the default mode, and `c-callout-kinds` with `--real-hosts`.
An Oh My Pi session, a Claude Code session and two Codex CLI sessions, each connecting to the hub with its own broker token, are admitted to their own channels and receive a direct message, again after a restart of the listener and again after a restart of the hub's server on the same store;
and a probe run in one session's own environment is refused another session's channel by name.

The stack is `packages/envoy/scripts/dev-stack.sh --callout`: the brokers, the listener, Dispatch and `nats-server` behind the NATS auth service, each the process production runs, built from this checkout.
dev-stack.sh restarts natsd (SIGUSR1) and the listener (SIGUSR2) in place, on the ports they served, and natsd on the same JetStream store and callout keys.
It runs its sessions two ways, each a step of the `nats-callout-sessions` part of `.github/workflows/ci-job-agent-harness.yaml` on a hosted runner, the default mode's first.

By default they are each plugin's own code under a stand-in host, in processes of their own and with no model:
the Oh My Pi extension under the stand-in host its own suite drives (`packages/pi-envoy/scripts/role-takeover/omp-client.ts`, whose heartbeat the script ticks before each message, as the host's timer would),
Claude Code's channel server under a stand-in `claude` that speaks MCP to it and plays the Legion inbox it writes each delivery to (`role-takeover/claude-client.ts --inbox`),
and the Codex plugin, `@sjawhar/codex-envoy`, built and packed as the release packs it (`scripts/build.ts --codex-out`, `bun pm pack`) and installed where Codex installs it, under a stand-in `codex` (`role-takeover/codex-client.ts`, compiled into a binary named `codex`).
The stand-in starts the plugin's MCP server from the installed manifest with only the manifest's `env_vars` and the variables Codex CLI 0.154.0 hands every MCP server, runs the plugin's hooks as Codex does (`SessionStart` at the first prompt and at `/new`), keeps the transcript the plugin reads, and answers the plugin's `codex queue`.

With `--real-hosts` they are the hosts themselves:
the Oh My Pi binary in RPC mode with this checkout's pi-envoy packed as the release packs it, the pinned Claude Code under `legion claude-bridge` (built from this checkout) with this checkout's claude-envoy,
and the pinned Codex CLI in a tmux server of the run's own with this checkout's codex-envoy packed as the release packs it and installed with Codex's own `codex plugin marketplace add` and `codex plugin add` into a fresh Codex home under `${XDG_CACHE_HOME:-$HOME/.cache}` (outside `TMPDIR`, where Codex refuses its helper binaries).
Each delivery may start a model turn, and every host calls the run's own model stand-in, [`lib/messages-standin.ts`](lib/messages-standin.ts) on loopback, which answers each turn with one end-turn text and refuses any request without the run's placeholder key; no check needs a model to call a tool.
Oh My Pi and Claude Code call it over Anthropic Messages (`ANTHROPIC_BASE_URL`, the model `claude-haiku-4-5`, which Oh My Pi runs as `anthropic/claude-haiku-4-5`),
and Codex over OpenAI Responses, through a model provider of its `config.toml` whose `base_url` is the stand-in's `/v1` and whose model, `standin-codex`, the stand-in's model listing names.
Before anything else starts, [`lib/model-endpoint.sh`](#libmodel-endpointsh)'s preflights hold the stand-in to the key: `model_key_preflight`'s `GET /v1/models` answers the key 200 and no key 401, and `responses_key_preflight`'s lists `standin-codex` to the key too.
The binaries are the pins the part's install actions install and export, `LEGION_TEST_OMP`, `LEGION_TEST_CLAUDE` and `LEGION_TEST_CODEX` (the table below), each held to its pin.
A missing input or tool refuses the run, naming it, before anything starts, on a runner as anywhere: no host is ever skipped.
It runs every check but `receives-after-switch`, which drives each host's switch through its stand-in host, and adds `transcript-in-evidence` for each Codex session (`17 of 17 checks passed (hosts: real)`).

In both, the Oh My Pi session is told `NATS_AUTH=broker` in its environment and the Claude Code session `"natsAuth": "broker"` in its `envoy.json`, so both ways of naming the credential run.
A Codex session can be told either way, so there are two: `codex-env`, told `NATS_AUTH=broker` in its environment alone, which reaches the plugin's MCP server only because the plugin's manifest names `NATS_AUTH` among its `env_vars`, and `codex-file`, told `"natsAuth": "broker"` in its `envoy.json`.
Each trades the stack's pod identity for a token naming its own session, which the script first registers for that pod, as a deployment's daemon does.

| check | what it holds |
| :--- | :--- |
| `receives (omp)`, `receives (claude)`, `receives (codex-env)`, `receives (codex-file)` | a direct message (`POST /v1/messages/send`) reaches the session: its own consumer (`s-<id>`, `/jsz`) acks the stream sequence it was delivered, which the listener does only on the host's `done` (a Codex session's once `codex queue` stored the message); the hub lists the connection as admitted as `session/<id>` (`/connz?auth=true`), and the host shows the message |
| `receives-after-listener-restart (…)` | the same, after the listener was restarted on its port |
| `receives-after-hub-restart (…)` | the same, after natsd was restarted on its port and store and the listener reconnected to it |
| `receives-after-switch (…)` | the same for the session ids each host switched to: Oh My Pi's `/new` (`omp-client.ts` `switch`), Claude Code's `/clear` (`claude-client.ts` `clear`, the handoff file the channel server adopts) and Codex's `/new` (`codex-client.ts` `new`, whose `SessionStart` hook writes that handoff file), each plugin moving its connection to the new id's token before it subscribes that id's subjects |
| `probe-refused-other-session` | `natsprobe` in the Oh My Pi session's environment is served its own channel and refused the Claude Code session's: `Permissions Violation for Subscription to "notifications.envoy.deliver.<id>"` |
| `probe-refused-other-session (codex-env)`, `(codex-file)` | `natsprobe` in the environment the Codex session's plugin server got (read from its `/proc/<pid>/environ`, so as the manifest's `env_vars` filtered it), presenting the credential the plugin's own settings resolve there ([`codex-probe.ts`](../../packages/pi-envoy/scripts/role-takeover/codex-probe.ts): the broker token, or none), is served its own channel and refused the Oh My Pi session's, and in the Oh My Pi session's environment it is refused the Codex session's |
| `transcript-in-evidence (codex-env)`, `(codex-file)` | `--real-hosts` only, read after the teardown: the `sessions/` the teardown left in the evidence holds the Codex session's own transcript, the file its receives read, whose first line names the session's id; the row copies nothing itself |

A receive that fails says where it stopped: `not admitted` when the hub lists no connection admitted as that session; `not acked` when the host shows the message but its consumer acked nothing new.
Four negative controls, `NATS_CALLOUT_SESSIONS_CONTROL`:
`seedless` starts every session with no credential named, so the auth service admits no connection and every receive FAILs `not admitted`, and each Codex probe row FAILs, its plugin's settings presenting no credential; the Oh My Pi probe row, which trades with the broker variables alone, passes;
`withhold-ack` (default mode) has the Claude Code stand-in take each delivery and never answer it, so the listener acks nothing: the first Claude Code receive FAILs `not acked` (the host shows it, its consumer acked nothing), the later ones FAIL `not acked` or held behind it, and the others, every probe row among them, pass;
`withhold-ack-codex` (default mode) has each Codex stand-in take each message the plugin's `codex queue` hands it and never answer, so that command does not return while the session runs and the listener acks nothing: the first receive of each Codex session FAILs `not acked`, the later ones FAIL `not acked` or held behind it, and the Oh My Pi and Claude Code ones and every probe row pass;
`manifest-drops-nats-auth` installs the Codex plugin with `NATS_AUTH` taken out of its manifest's `env_vars`, so Codex hands `codex-env`'s server no `NATS_AUTH` and it dials with no credential: `codex-env`'s receives FAIL `not admitted`, naming the manifest, its probe row FAILs, presenting none, and the others pass.
Each exits 1.

| input | default | meaning |
| :--- | :--- | :--- |
| `LEGION_TEST_PG_DSN`, else `DATABASE_URL` | none: refused | the Postgres server dev-stack.sh creates its two databases on and drops on exit |
| `LEGION_TEST_OMP` | none: `--real-hosts` refuses | the `omp` of the build `.omp-pin` names, which `.github/actions/install-omp` installs and exports; its `--version` is held to `.omp-pin`'s release and a mise install of oh-my-pi to its build |
| `LEGION_TEST_CLAUDE` | none: `--real-hosts` refuses | the Claude Code of the release `.claude-code-pin` names, by an absolute path, which `.github/actions/install-claude-code` installs and exports; its `--version` is held to that release |
| `LEGION_TEST_CODEX` | none: `--real-hosts` refuses | the native `codex` of the release `.codex-pin` names, which `.github/actions/install-codex` installs and exports; `codex --version` must print `codex-cli <release>` (tested with 0.154.0) |
| `NATS_CALLOUT_SESSIONS_CONTROL` | unset | `seedless`, `withhold-ack`, `withhold-ack-codex` or `manifest-drops-nats-auth`, the negative controls |
| `NATS_CALLOUT_SESSIONS_EVIDENCE_DIR` | a fresh directory under `TMPDIR` | kept and printed: `dev-stack.log`, `stack-logs/` (every server's log, each restarted one's earlier log beside it), `client-logs/` (each client's stdout and stderr and the channel server's stderr, and with `--real-hosts` the model stand-in's log), `answers/` (each send's answer, each acking consumer's state and the probe's output, and with `--real-hosts` `standin-requests.jsonl`, each request the model stand-in answered or refused) and, with `--real-hosts`, `sessions/` (each host's transcript and logs) |

By default it needs `go` (unless `--binaries`, passed on to dev-stack.sh, names the stack's binaries), `bun` with this checkout's `node_modules`, `psql`, `curl`, `jq` and `python3`; `--real-hosts` needs `tmux`, `setsid` and `mkfifo` too.
In either mode it takes no container, no credential and no model from outside the runner, so the agent-harness job's `nats-callout-sessions` part runs both on a hosted runner.
No `AGENT_SECRETS_*`, `DISPATCH_*`, `ENVOY_*`, `NATS_*`, `OMP*`, `PI_*`, `ANTHROPIC_*`, `CLAUDE_*`, `CODEX_*`, `OPENAI_*` or `LEGION_TEST_*` variable of the caller reaches anything the run starts (the DSN and the three binaries are read first); with `--real-hosts` the stand-in's placeholder key reaches the hosts' environments alone, never an argv.
Each check prints `CHECK <check>: PASS`, with what it read on the line after it, or `CHECK <check>: FAIL: <reason>`.
Exit status: 0 every check passed; 1 a check failed; 2 refused (an input or tool is missing, a pin does not match, or the stack or a client did not start).

## lib/loopback-dsn.py

Prints what libpq reads from a PostgreSQL DSN, rewrites that DSN to name a run's own database, or refuses the DSN in one line.
Python 3.9 or later, the standard library, and libpq itself, which it loads with `ctypes`.
Eight scripts read a DSN with it before they start anything, each of which creates, resets or drops something on the server its caller's DSN names.
Each is a test of its own:
[`verification-results.sh`](#verification-resultssh), [`dispatch-claimant-delivery.sh`](#dispatch-claimant-deliverysh), [`stage2-tmux-supervision.sh`](#stage2-tmux-supervisionsh), [`controller-start-tmux.sh`](#controller-start-tmuxsh), `clean-session-stop-tmux.sh`, [`envoy-session-delivery.sh`](#envoy-session-deliverysh), [`dispatch-session-settings.sh`](#dispatch-session-settingssh) and [`dispatch-user-turns.sh`](#dispatch-user-turnssh).
Each call calls it in one line, through [`lib/loopback-dsn.sh`](#libloopback-dsnsh)'s `require_loopback_dsn` or through its `run_database_url`, which begins with the same check.
Six of the eight source that file through [`lib/rig.sh`](#librigsh), and [`stage2-tmux-supervision.sh`](#stage2-tmux-supervisionsh) and [`envoy-session-delivery.sh`](#envoy-session-deliverysh) source it on their own:

```sh
require_loopback_dsn db_host DATABASE_URL "$database_url" "this run resets the database" || fail "$db_host"
```

[`envoy-session-delivery.sh`](#envoy-session-deliverysh), [`dispatch-session-settings.sh`](#dispatch-session-settingssh) and [`dispatch-user-turns.sh`](#dispatch-user-turnssh) read their DSN with `run_database_url` alone, in the step that names the run's own database.
Every proof that writes a server its caller's DSN names reads that DSN with this guard before it starts anything.
The daemon's tests that drive real Oh My Pi panes read no DSN in the shell: their test binary reads every DSN it builds from `LEGION_TEST_PG_DSN` back with pgx (`DatabaseURL`, `packages/daemon/internal/testpg/testdb/testdb.go`).
The rationale is written once, above `require_loopback_dsn` in [`lib/loopback-dsn.sh`](#libloopback-dsnsh): the client the run then writes that server with must be libpq too, because libpq and a URL parser read different text as the host.
That one copy also says which client each call site then writes the server with, what the pinned `github.com/jackc/pgx/v5 v5.11.0` carries for the two proofs that give Dispatch the caller's raw DSN and for the binary's own rewrites, and what a raise of that pin moves.

A caller clears `PGHOSTADDR`, `PGSERVICE` and `PGSERVICEFILE` before its first connection, because this helper reads the DSN alone.
psql reads `PGHOSTADDR`, and `PGSERVICE` with `PGSERVICEFILE` name an entry of `~/.pg_service.conf` that can set `hostaddr`.
Measured with an exported entry that sets `hostaddr=127.0.0.2`: psql connected to 127.0.0.2, and the helper printed `127.0.0.1` for the same DSN.
All eight drop the three before their first connection: five in the sweep of the environment they do not inherit, and `stage2-tmux-supervision.sh`, `controller-start-tmux.sh` and `dispatch-user-turns.sh` in an `unset` of their own.

`loopback-dsn.py NAME WHAT` reads the DSN from the environment variable NAME, never from the command line, so no process list holds its password.
WHAT is what the run does on that server, in a few words, and a refusal about the host ends with it.
The helper prints the host and exits 0 when libpq reads one loopback host from the DSN: `127.0.0.1`, `localhost` or `::1`.
Every other answer is a refusal: one line on stderr and exit 1.
The host is the one libpq reads, wherever the DSN puts it: `postgres://localhost:5433/db?host=127.0.0.1` is accepted and its host is `127.0.0.1`, and `postgres://evil.example?x=@localhost` is accepted and its host is `localhost`, since libpq reads `evil.example?x=` there as the user name.

libpq's own parser reads the DSN (`PQconninfoParse`), because a shell pattern and libpq disagree about which text is the host.
libpq ends the user name at the first `@`, and a `?` does not end it, so the host of `postgres://localhost?sslmode=disable@evil.example/db` is `evil.example`, while a pattern that ends the authority at the first `?` reads `localhost`.
libpq decodes a percent sign in a parameter key too, so `%68ost=` reaches it as `host=`, and it reads the whole DSN, while a reader that takes the parameters one line at a time reads the first line alone.
A WHATWG `URL` ends the user name at the last `@` and starts the query at the first `?`, so it reads the host of `postgres://u@localhost:5432@10.1.2.3/db` as 10.1.2.3 and the host of `postgres://evil.example?@localhost/db` as evil.example, where libpq reads `localhost` in both and the helper prints it.
Measured with psql 18.6: the first names the port `5432@10.1.2.3`, which psql refuses before it connects anywhere, and the second connects to `localhost (127.0.0.1)`.

`loopback-dsn.py NAME WHAT --dbname DATABASE` prints the connection string that gives a run its own database on the host the DSN names.
The proofs reach it through `run_database_url` in [`lib/loopback-dsn.sh`](#libloopback-dsnsh), which all eight source.
The string is the keywords libpq reads from the DSN, with DATABASE as `dbname`, written as `dbname='…' host='…'`: the keyword/value form libpq and pgx read wherever they read a URL.
It is rebuilt and not appended to.
A `?dbname=` or `&dbname=` appended to the DSN's own text takes its separator from a `?` that can be inside the user name, where it starts no query, and it breaks a DSN whose text ends in a `?`.
A join of the DSN's server, the database and the DSN's parameters is worse: a `?dbname=other` the caller's DSN carries outranks the path for libpq and for pgx, so the run migrates `other`.
libpq reads the rebuilt string back before the helper prints it: libpq must read DATABASE as the database, the same host as the DSN names, and every other keyword as the DSN sets it.
Every value reaches that string as libpq decoded it, so this mode also refuses a value that holds a character that does not print: a newline in a password is a line break in a string the caller writes into a file, and a byte that is no UTF-8 text is a surrogate (`U+DCFF` for the byte `0xFF`) that no file carries back to psql.
A refusal names the database, the host, and every keyword libpq does not read back as the DSN sets it.

The refusals are a character that does not print in the DSN (`str.isprintable()` is false for the C0 controls, DEL, the C1 controls, the bidirectional format characters and the surrogates), a host, `hostaddr` or `service` holding a character that does not print after libpq decodes it, a DSN libpq cannot parse, a DSN with no host, a host list of more than one (libpq connects to the next host when the first does not answer), a `hostaddr` parameter, a `service` parameter (whose entry in `~/.pg_service.conf` can set `hostaddr`, which sends the connection to another address), and a host that is none of the three loopback names.
The `--dbname` mode refuses one more: any keyword whose value holds such a character, since it writes every value back.
Each refusal names what libpq read — the host, the hostaddr or the service, or else the keyword alone — and prints no other part of the DSN, because the user name and the password are in it.
It prints a value only where every character of the value prints, so a refusal is one line and writes no escape sequence to the terminal.
Each value is decoded with `errors="surrogateescape"`, which round-trips every byte, so host mode takes the DSNs libpq takes, whatever bytes a password holds: it writes no value back.
Where `ctypes.util.find_library("pq")` finds no libpq, every DSN is refused and the refusal names libpq.
There is no second, weaker check to fall back on.

`loopback-dsn.test.ts` drives the helper with this host's own libpq and asserts the answer it gave, never a pattern of the DSN:
the DSN a run is given, `localhost` and `::1`, a `host=` parameter, a host name inside the user name, an empty `hostaddr=`, the two forms with a `?` inside the user name, a second `@` inside the authority, a `?` right before the `@`, a password of bytes that are no UTF-8 text, a `%68ost=` key, a Unix-socket directory, a newline before an `&host=`, a percent-encoded newline, a percent-encoded escape sequence and a percent-encoded C1 control in the host, two hosts, a `hostaddr`, a `service`, a remote host, a DSN with no host, a DSN libpq cannot parse, and an unset variable.
It drives `run_database_url` over seven DSNs — a `?` in the user name, a host name in the user name, a DSN whose text ends in a `?`, the DSN a run is given, the two a WHATWG `URL` reads another host from, and one whose query sets `dbname` — and pins the string the rewrite answers with, which libpq has read back inside the helper.
Two more cases pin a refusal of that mode: a password of bytes libpq reads as no UTF-8 text, and a percent-encoded newline in a password.

## lib/loopback-dsn.sh

The two helpers a proof that writes a PostgreSQL server reads its caller's DSN with, and the one copy of why they ask libpq.
Both answers come from [`lib/loopback-dsn.py`](#libloopback-dsnpy), which this file addresses beside itself, so it needs nothing of its caller's.
A third helper, `empty_public_schema`, empties the database a run was given, so that a server migrates it from nothing.
[`lib/rig.sh`](#librigsh) sources this file, which is how six of the eight scripts that call its helpers have it, and [`stage2-tmux-supervision.sh`](#stage2-tmux-supervisionsh) and [`envoy-session-delivery.sh`](#envoy-session-deliverysh) source it on their own; `envoy-session-delivery.sh` keeps an `await_port_file` and a `keep_port` of its own and sources no rig.

```sh
. "$root/scripts/e2e/lib/loopback-dsn.sh"     # sourced, never run
```

- `require_loopback_dsn VAR NAME DSN WHAT`

  assigns VAR the host libpq reads from DSN, which is `127.0.0.1`, `localhost` or `::1`, and returns 0.
  For a DSN [`lib/loopback-dsn.py`](#libloopback-dsnpy) refuses it assigns VAR that refusal line and returns 1, so each caller ends the run in its own words (`|| fail "$db_host"`).
  NAME is the environment variable the caller read DSN from, and it starts the refusal, and WHAT is what the run does on that server, in a few words.
  The comment above it is the one copy of why every proof asks libpq, which clients the proofs then write the server with, and what a raise of the pinned pgx moves

- `run_database_url VAR NAME DSN DATABASE WHAT`

  assigns VAR the connection string that names the run's own database DATABASE on the server DSN names, so nothing the run starts writes the database the caller's DSN itself names, and returns 0.
  For a DSN the helper refuses it assigns VAR that refusal line and returns 1, the contract `require_loopback_dsn` has, so the caller chooses how the run ends.
  [`lib/loopback-dsn.py`](#libloopback-dsnpy) rebuilds that string from the keywords libpq reads and reads it back before it prints it, and NAME, the variable the caller read DSN from, starts the refusal a DSN it will not take earns.
  WHAT is what the run does on that server, in a few words.
  This mode refuses every DSN the guard refuses, and a value the rebuilt string cannot carry as well, so [`envoy-session-delivery.sh`](#envoy-session-deliverysh), [`dispatch-session-settings.sh`](#dispatch-session-settingssh) and [`dispatch-user-turns.sh`](#dispatch-user-turnssh) call it in their setup step as their one read of the DSN, before any build or service

- `empty_public_schema URL`

  drops every table, view, routine and type of the `public` schema of the database URL names, and keeps the extensions that schema holds.
  It returns the status of its `psql`, so the caller ends the run in its own words (`|| fail "could not empty the database's public schema"`).
  The cluster's master role installs `vector` (`0072_embeddings_core.up.sql` takes it as given with `create extension if not exists`), and the role a run connects as need not be that role.
  A `drop schema public cascade` would remove `vector` with the schema and leave a database no build can migrate.
  A Legion pod's test server is one such case: every database made from its `template1` holds `vector`, and its login may not create it again.
  [`verification-results.sh`](#verification-resultssh) and [`dispatch-claimant-delivery.sh`](#dispatch-claimant-deliverysh) call it

`require_loopback_dsn` and `run_database_url` share one call to `lib/loopback-dsn.py`, which takes the DSN from the environment of a command substitution's own subshell, so no process list holds the password.

## lib/held-port.py

Starts a server on a TCP port the kernel chose, and holds that port from the choice until the server serves on it, so nothing else can take it in between.
Python 3.9 or later, standard library only.
It replaces picking a port that was free a moment earlier and passing its number:
such a port can be taken before the server binds it, and the bind then fails with `address already in use`.
The server must adopt an inherited listening socket:
`ENVOY_LISTEN_FD` (envoy-listener), `DISPATCH_LISTEN_FD` (envoy-dispatch) or `LEGION_LISTEN_FD` (`legion start`).

```sh
# One server: the helper listens on 127.0.0.1:0 and becomes the server.
python3 scripts/e2e/lib/held-port.py exec --port-file "$work/dispatch.port" \
  --fd-env DISPATCH_LISTEN_FD \
  --env DISPATCH_PORT={port} --env DISPATCH_SERVER_URL=http://127.0.0.1:{port} \
  -- "$work/envoy-dispatch" &
server_pid=$!                       # the server's own PID: the helper executes it in place

# A port that outlives restarts: a keeper holds it, and each start receives a copy.
python3 scripts/e2e/lib/held-port.py keep --port-file "$work/listener.port" --socket "$work/listener.sock" &
keeper_pid=$!                       # wait for listener.port before the first start
python3 scripts/e2e/lib/held-port.py exec --from "$work/listener.sock" \
  --fd-env ENVOY_LISTEN_FD --env PORT={port} -- "$work/envoy-listener" &
```

`exec [--host H | --from SOCKPATH] [--port-file F] --fd-env NAME [--env K=V]... -- CMD [ARG]...` opens a listening socket on `H:0` (default `127.0.0.1`;
an IPv6 address such as `::1` works) or, with `--from`, receives one from the keeper at `SOCKPATH`.
It puts the socket on descriptor 3, inheritable, and keeps no other copy.
It replaces `{port}` in each `--env` value and in CMD and its arguments, sets each `K=V` and `NAME=3`, writes the port and a newline to F (a temporary file in F's directory, then a rename, so a reader never sees half a number) and executes CMD in its own process: `$!` is the server's PID.
A failure before CMD runs exits 1 and names what failed on stderr; a CMD that cannot be executed exits 127 (not found) or 126, after F was written.
Either way the socket closes with the process and the port is free again.

`keep [--host H] --port-file F --socket SOCKPATH` holds one listening socket for its whole life and serves the unix socket `SOCKPATH`, answering each connection with a copy of the held socket (SCM_RIGHTS).
It writes F only once `SOCKPATH` listens, so a reader of F can start an `exec --from` at once.
Every server started from it serves the same port, and a restart cannot lose the port to another process.
It runs until killed: SIGTERM or SIGINT exits 0 and removes `SOCKPATH`; the caller's cleanup kills it.

Between two servers, a kept port still listens.
The kernel accepts a connection into the socket's queue and nothing answers it until the next server starts and accepts it.
A check that the server is down therefore sees a hang, not a refusal: poll with a timeout (`curl --max-time 2`) and take the timeout for down.

`held-port.test.ts` runs the real envoy-listener of this checkout through both commands:
it serves on the port the port file names in the helper's own PID, a competing bind is refused before the server runs, the listener holds the socket only on its own close-on-exec copy, a command that cannot run frees its port, and a keeper keeps its port, unanswered, between two listeners.
It builds the listener with `go`, so CI runs it after `setup-go`.

Every script and rig takes its servers' ports this way.
[`lib/rig.sh`](#librigsh)'s `start_held`, `keep_port` and `start_kept` wrap it for the scripts that source the rig (Stage 1, `controller-start-tmux.sh`, `clean-session-stop-tmux.sh`, `dispatch-user-turns.sh`, the docs rigs and the skill-scenarios rig);
Stage 2 calls it directly.
A daemon always serves on a keeper's socket, since its `legion.yaml` must name the port before it starts.
A process that binds port 0 itself and reports what it got
— `nats-server -p -1`, a Bun fake, `kubectl port-forward :<port>` — needs no helper; the rig reads its report with `report_port`.
A port a person sets explicitly is used as given, and an occupied one fails with `address already in use`.

## lib/port-file.sh

The wait for the port file [`lib/held-port.py`](#libheld-portpy) writes, which `lib/rig.sh`'s `start_held` and `keep_port`, `envoy-session-delivery.sh`, [`nats-callout-sessions.sh`](#nats-callout-sessionssh) (for its model stand-in's port file), and `packages/envoy/scripts/search-smoke.sh` and `searchbench.sh` share.
Sourced, never run; it needs nothing of its caller's.

```sh
. "$root/scripts/e2e/lib/port-file.sh"
read_port_file "$work/dispatch.port" "$server_pid" port || waited=$?
```

`read_port_file FILE PID VAR` polls FILE every tenth of a second while PID lives and assigns VAR the port it names.
It prints nothing, so each caller words its own failure:
it returns 0 once VAR is set, 1 when PID exits before FILE is written, and 2 after `port_file_seconds` (30).

## lib/built-from.sh

Prints what a script ran, so a run can be tied to a commit after its scratch directory, and the binary in it, are gone.
Stage 1, Stage 2 and `clean-session-stop-tmux.sh` print it through their own `note`.

```sh
bash scripts/e2e/lib/built-from.sh "$root" "$work/legion"
# source: <commit> on <parent>                     (jj: the working copy and its parent; git: HEAD)
# legion: sha256 <hash>, stamped <commit> modified=<true|false>
```

When the working copy has changes, as in a negative control, which runs a base with the new script copied in, the source line says so.
The next line gives the sha256 of the diff, followed by its `--stat`, so the run shows what it held and not only that something changed.

- Under jj the diff is `jj diff --git`.

- Under git it is `git diff HEAD`, taken against a copy of the index in which every untracked file is added as intent-to-add.
  The copied-in script is therefore in it, and the checkout's own index is never written.
  Without jj, or outside a jj workspace, it reads git, which is what CI's checkout is.

Each binary is tied to that source by the stamp the Go toolchain writes at build time.
The stamp has two fields: `vcs.revision`, the commit the checkout's git HEAD named, and `vcs.modified`, whether the tree differed from it.
Under jj, HEAD is the working copy's first parent.
The helper fails, naming both sides, when a binary:

- carries no stamp (`-buildvcs=false`, or built outside a checkout);

- was stamped with a commit that is not the source's;

- has a modified flag that disagrees with the tree now.

The stamp does not hash a changed tree, so an edit made after the build to a tree that was already changed goes unseen.
Every caller runs the helper right after its build.

## lib/pack-plugin.sh

Packs one of this checkout's two plugins, `@sjawhar/pi-envoy` (`packages/pi-envoy`) or `@sjawhar/pi-legion` (`packages/pi-legion`), the way the release packs it, and prints the tarball's path:
what `npm pack` ships (`package.json` `files`:
`dist/` with the one bundle and the prepack's `dist/skills`, the Legion plugin's `agents/`, and the packed manifest).
Every script that installs a branch-built plugin packs through it:
[`lib/install-plugin-profile.sh`](#libinstall-plugin-profilesh), which `controller-start-tmux.sh`, `stage2-tmux-supervision.sh`, `dispatch-user-turns.sh` and the skill-scenario rig (`packages/pi-legion/scripts/skill-scenarios/rig.sh`) call.
The worker image packs on its own, as the release does:
`packages/daemon/docker/worker.Dockerfile`'s two plugin `RUN`s each rewrite `omp.extensions` with `jq` and run `bun pm pack`, and the prepack (`scripts/pi-plugin-prepack.sh`) refuses to pack any other `omp.extensions`, which holds all of them to the same manifest.

```sh
bun install --frozen-lockfile     # once, at the workspace root: the bundle resolves @legion/* there
envoy=$(scripts/e2e/lib/pack-plugin.sh pi-envoy "$work/pack/pi-envoy")
legion=$(scripts/e2e/lib/pack-plugin.sh pi-legion "$work/pack/pi-legion")
```

The first argument is the package's directory under `packages/`, `pi-envoy` or `pi-legion`;
any other is refused, since the other workspace packages are not plugins.
`<out dir>` is created when missing, and refused inside the checkout (jj would snapshot the tarball) or when it already holds a `.tgz`.
Stdout is exactly one line, the tarball's path; every step's own output goes to stderr.

The steps are the release's, run in the checkout
— a copy of the package cannot build, because the prepack copies the repository's `skills/` and the bundle resolves `@legion/*` through the root's `node_modules`:

1. save the package's `package.json` and arm an `EXIT` trap that copies it back byte-identical (`.github/workflows/release.yaml`'s `pi_envoy_build` and `pi_legion_build` jobs rewrite the manifest in "Point extensions at the packed bundle" and discard their tree, so only this script restores);

2. rewrite `omp.extensions` to `["dist/<entry>.js"]` with `jq`, where `extensions/<entry>.ts` is the committed manifest's one entry (the same step, and the `jq '.omp.extensions = …'` line of `packages/daemon/docker/worker.Dockerfile`'s plugin `RUN` for that package);

3. `bun pm pack --destination <out dir>`, whose `prepack` builds `dist/` (the release's "Pack extension" step, `scripts/pi-plugin-prepack.sh`);
   the bundle inlines `package.json`, so it is built while the manifest names the packed bundle, as the release builds it;

4. copy the saved manifest back and check it byte for byte (this script's own need: the checkout is reused, where the release's build job tree is discarded).

Each step cites its source by what it runs, never by line number: the lines move with every edit above them.
The release's version bump (its "Set release version" step) is not a step: the tarball carries the checkout's own version.
The saved manifest is written to the run's `mktemp -d` directory, never beside `package.json`, so an interrupted run strands no `tmp.json` in the checkout.

The manifest is rewritten only for as long as the pack takes.
The trap copies it back on every other way out
— a failed step, `SIGHUP`/`SIGINT`/`SIGTERM` (each routed through `exit`) — so a pack that dies halfway never leaves the rewrite for jj to snapshot.
The script ignores SIGPIPE, so this holds when whoever read its stderr has gone too:
bash's `Terminated` notice for an interrupted pack would otherwise kill it before the trap ran.
It keeps the run's status; if the copy back itself fails, it says where the saved bytes are, leaves them there, and exits non-zero.
Afterwards `jj status` is as it was before the run: `dist/` is gitignored, and nothing else is written inside the checkout.

Runs in one checkout take turns at each package's manifest from the save to the copy back, under a `flock` on the manifest itself (rewritten and restored in place, so the lock's inode lasts the whole window);
a run that has to wait says so on stderr.
Without the lock, a run that starts while another has the manifest rewritten saves that rewrite as its "before" and puts it back at its own exit:
both runs exit 0 and jj snapshots the rewritten `package.json`.
Two stage proofs in one checkout, or a stage proof and the skill-scenario rig, can pack at the same time, and the lock takes them in turn;
the two packages' packs never wait on each other.

## lib/install-plugin-profile.sh

Installs one of this checkout's two plugins, `@sjawhar/pi-envoy` or `@sjawhar/pi-legion`, into a named OMP profile, packed the way the release packs it, so a stage proof runs the branch-built plugins and the user's own profiles are never touched.
A Legion pane needs both — the daemon's boot gate refuses `pi-legion` without `pi-envoy` — so every stage calls it twice, with a `--dest` of its own each time.

```sh
bun install --frozen-lockfile     # once, at the workspace root: the bundle resolves @legion/* there
. scripts/e2e/lib/omp-home.sh && make_omp_home "$work/omp-home"
envoy_manifest=$(scripts/e2e/lib/install-plugin-profile.sh --package pi-envoy --profile legion-e2e-$$ --home "$work/omp-home" --dest "$work/pi-envoy")
manifest=$(scripts/e2e/lib/install-plugin-profile.sh --package pi-legion --profile legion-e2e-$$ --home "$work/omp-home" --dest "$work/pi-legion")
# → $work/omp-home/.omp/profiles/legion-e2e-<pid>/plugins/node_modules/@sjawhar/pi-envoy/package.json
# → $work/omp-home/.omp/profiles/legion-e2e-<pid>/plugins/node_modules/@sjawhar/pi-legion/package.json
```

Each item below names a flag, then what it means.

- `--package <pi-envoy|pi-legion>`

  the plugin to install, by its directory under `packages/`; any other value is refused, since the other workspace packages are not plugins.
  The listing below is checked by that package's npm name.

- `--profile <name>`

  the OMP profile to install into (`OMP_PROFILE=<name>`).
  Refused when OMP would read it as its default profile — empty, all whitespace, or `default` — since that is the profile every plain `omp` uses.
  Any other name goes to OMP as given, and OMP refuses one it cannot use.

- `--home <dir>`

  the `HOME` Oh My Pi runs under for the profile, which is then `<dir>/.omp/profiles/<name>`: the run's own home from [`make_omp_home`](#libomp-homesh).
  Refused when it is the caller's own `HOME`, when `HOME` is unset or empty (it could not be compared), and when `<dir>/.omp` does not exist.

- `--dest <dir>`

  where the tarball is unpacked.
  `omp plugin install` links this directory into the profile rather than copying it, so it **is** the installed plugin and must outlive the run.
  Refused inside the checkout (jj would snapshot it, symlinks resolved first) and when it exists and is not an empty directory (an unpack over an earlier build would keep that build's stale files).

- `--omp <mise tool spec>`

  optional: the Oh My Pi build steps 2 and 3 run (`github:cybermonklabs/oh-my-pi@<version>`), `.omp-pin`'s by default.
  Stage 2 passes the build its daemon and panes run, so no other build opens the profile.
  Refused when empty.

The first four flags are required; each refusal names its flag and exits 2.
Stdout is exactly one line, the installed manifest's path as `HOME=<home> OMP_PROFILE=<name> omp plugin list --json` reports the plugin;
for `pi-legion` that is the manifest the daemon's contract gate reads under the same profile (`pluginManifestPath`, `packages/daemon/internal/daemon/bootgate.go`).
Every step's own output goes to stderr.

The plugin is packed by [`lib/pack-plugin.sh`](#libpack-pluginsh), the release's pack steps run in the checkout, into the run's `mktemp -d` directory (never beside `package.json`, so an interrupted run strands no `.tgz` in the checkout).
Then:

1. unpack the tarball into `<dir>` (`worker.Dockerfile`'s `mkdir -p /out/<package>` and `tar xzf ./*.tgz -C /out/<package> --strip-components=1`);

2. `OMP_PROFILE=<name> omp plugin install <dir>` (`worker.Dockerfile`'s `omp plugin install /opt/legion/<package>`);

3. `OMP_PROFILE=<name> omp plugin list --json` must show the package, by its npm name, at the tarball's version, enabled, and resolving to `<dir>`.

Steps 2 and 3 run the Oh My Pi `--omp` names, else the one the daemon pins (`.omp-pin`), under `HOME=<home>`, from `<dir>`, as the daemon runs it:
the build's own `bin/omp` by its absolute path (`mise where <pin>`) under `mise x <pin> --`, never an `omp` looked up on the caller's `PATH`.
An operator's wrapper there (a dotfiles shim such as `~/.dotfiles/shims/omp`) reads its own files from `HOME`, which is the run's, and its own setup could rewrite the profile.

Each step cites its source by what it runs, never by line number: the lines move with every edit above them.

The script creates the profile and `<dir>` and removes neither;
the caller does, with its work directory, which holds both (the profile holds `plugins/` — the link and `omp-plugins.lock.json` — and OMP's `logs/`).

### The natives download

OMP's native module (`pi_natives.linux-x64-modern.node` or `…-baseline.node`, the one the CPU selects, 123 MiB at the pin;
builds before 18.11.0 wrote every variant they embed, about twice that) lives in `$HOME/.omp/natives/<omp version>/`, and every profile under that `HOME` shares it; a profile has no natives of its own.
`getNativesDir` (`@oh-my-pi/pi-natives/native/loader-state.js`) takes the first of `PI_NATIVES_DIR`, `$OMP_DOWNLOAD_ROOT/natives`, `$XDG_DATA_HOME/omp/natives` when `$XDG_DATA_HOME/omp` exists, and `natives/` under `OMP_CONFIG_ROOT`, else under `$HOME/.omp`; the pinned build reads all of them, and none of `PI_NATIVES_DIR`, `OMP_DOWNLOAD_ROOT` and `OMP_CONFIG_ROOT` is set by any Legion process.
OMP writes it on its first run under a `HOME` that has not run this OMP version, and in this script that run is `omp plugin install`.
The run's own home links `.omp/natives` to the operator's ([`lib/omp-home.sh`](#libomp-homesh)), so a fresh CI runner, a container, or a newly bumped OMP pin pays that once, into the operator's cache;
on a box where the pinned OMP has already run, a fresh profile pays nothing.
Measured with OMP 18.2.2: build, pack, install and verify took 2 s into a new profile, the profile got no `natives/` directory, and `~/.omp/natives/18.2.2/` was untouched.

## lib/model-gateway-url.sh

Prints `LEGION_E2E_MODEL_GATEWAY_URL`, the model gateway's Anthropic endpoint, once it is one a run can use.
[`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) reads the variable through it.
The repository carries no default: the operator sets it to the `baseUrl` of the `anthropic` provider in their own gateway route (`~/.omp/agent/models.yml`).

```sh
gateway=$(bash scripts/e2e/lib/model-gateway-url.sh)
```

The URL is written into an Oh My Pi `models.yml` as one plain YAML scalar, so the helper exits 1 when the variable is unset, holds a character other than letters, digits and `:/._~-`, ends in a colon (which YAML reads as a mapping key), or is not an `https://` URL.
Its refusal names the variable on stderr and never prints the value, which names production infrastructure;
the scripts that use it print that the route comes from the variable, not the URL.

## lib/install-model-gateway.sh

Routes a named OMP profile's model turns to the Hawk model gateway (middleman) on the operator's own hawk login, the route an operator's own agent sessions use when their machine is configured for the gateway (`~/.omp/agent/models.yml`:
`X-Api-Key: !hawk-token`), so a run's panes reach Anthropic with no provider key.
Its caller is pi-legion's skill-scenario rig, a tool (`packages/pi-legion/scripts/skill-scenarios/rig.sh`, its `gateway` route), which runs it under the operator's own `HOME`.

```sh
export LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic
key_command=$(bash scripts/e2e/lib/install-model-gateway.sh --profile legion-e2e-$$ --home "$work/omp-home" --dest "$work/model-gateway" --cache-dir "$work/model-gateway-cache")
# → $work/model-gateway/hawk-token
```

`LEGION_E2E_MODEL_GATEWAY_URL` is required:
the gateway's Anthropic endpoint, the one `hawk-token`'s default `HAWK_API_URL` mints keys for, read through [`lib/model-gateway-url.sh`](#libmodel-gateway-urlsh).

Each item below names a flag, then what it means.

- `--profile <name>`

  the OMP profile to route.
  Refused when OMP would read it as its default profile (empty, all whitespace, or `default`), and when its `agent/models.yml` or `agent/config.yml` already exists.

- `--home <dir>`

  the `HOME` Oh My Pi runs under for the profile, whose files then go under `<dir>/.omp/profiles/<name>/agent`:
  the run's own home from [`make_omp_home`](#libomp-homesh).
  Refused when it is the caller's own `HOME`, when `HOME` is unset or empty (it could not be compared), and when `<dir>/.omp` does not exist.

- `--dest <dir>`

  where the key command and its log are written; created `0700`.
  Refused when it exists and is not an empty directory, and when its path holds a character other than letters, digits, `/`, `.`, `_` or `-`, since it is written into YAML as one `!command` word.

- `--cache-dir <dir>`

  where the key command keeps the key it minted; created `0700`.
  Refused when it exists and is not an empty directory, so a key left there is never served.
  A private directory of the run, never its evidence: the key is a live gateway credential.

It writes `<dir>/hawk-token`, the key command:
`hawk-token` (resolved on `PATH`) run under the caller's `HOME`, `DBUS_SESSION_BUS_ADDRESS` and XDG base directories (a variable the caller has unset is unset for it), for that one command.
It appends one line per invocation, one per mint, one per call that got no key and why, and `hawk-token`'s own stderr to `<dir>/hawk-token.log`, and one line per call and its outcome to `<dir>/hawk-token.calls` ([`lib/model-gateway-unserved.sh`](#libmodel-gateway-unservedsh) reads it); stdout carries the key alone.
The profile's `agent/models.yml` points the `anthropic` provider at `LEGION_E2E_MODEL_GATEWAY_URL` with `apiKey` and `X-Api-Key` both `!exec <dir>/hawk-token` (Oh My Pi runs a `!command` through `/bin/sh -c`, and `exec` makes the key command the process it started, so the call's clock starts when Oh My Pi started it), and its `agent/config.yml` pins every model role, the roles Legion's task agents name included, to `anthropic/claude-opus-4-8`, sets `enabledModels: [anthropic/*]`, disables `amazon-bedrock`, `bedrock-mantle`, `google`, `ollama`, `llama.cpp` and `lm-studio`, and sets `startup.setupWizard: false`, so Oh My Pi opens no setup wizard in the fresh profile, whatever setup version its build counts to.
Stdout is the key command's path.

A pane cannot run `hawk-token` itself, which is why the command, and only it, gets the operator's environment.
Measured in a Go pane at `f1749048` whose profile named `!hawk-token` directly, by running `hawk-token` under that OMP process's exact environment: it fails on mise (`No version is
set for shim: uv`), because the pane's `XDG_CONFIG_HOME` is the daemon's own (LEGION-206 P1) and mise's global config is not there;
with the operator's XDG directories it fails on the login (`no
usable hawk login`), because the pane carries no `DBUS_SESSION_BUS_ADDRESS`, the only address the keyring client reads (`jeepney/bus.py`, `find_session_bus`); with both it mints.
A pane's `HOME` is the run's own ([`lib/omp-home.sh`](#libomp-homesh)), where neither mise's global config nor the hawk login is, so the command gets the operator's `HOME` too.
Panes run as the operator's uid and can reach `/run/user/<uid>/bus` anyway, so the command gains nothing a pane lacks, and every pane's environment stays as it is.

Oh My Pi falls back from a failing provider without a word, so the profile leaves it nowhere to fall.
In that same pane, OMP logged `model-config: !command value resolution failed` and answered from `amazon-bedrock/us.anthropic.claude-opus-4-8` on the host's instance role, and an earlier retro's scout subagent ran on Bedrock's `openai.gpt-oss-120b`.
`enabledModels` holds each session's own model to `anthropic`: a pane whose key command fails refuses to start (`No model
available matching enabledModels (anthropic/*) with usable credentials`), and the claim fails its launch budget.
Subagents and retries choose from every enabled provider rather than that list (OMP's `resolveModelOverride` reads `getAvailable()`), so every provider a pane can use without the gateway is disabled:
in a pane's environment with `GEMINI_API_KEY` set, `omp models` lists `amazon-bedrock`, `bedrock-mantle`, `google` and `anthropic` when the file sets only the default role, and `anthropic` alone with the file as written;
the three local servers are ones OMP uses with no key.
Every role is the one model because the gateway answers `claude-haiku-4-5`, the model OMP gave that scout once Bedrock failed it, with `404 model not found`.

Its first mint is the preflight, before any pane exists.
It exits 1 naming the cause when `hawk-token`, `flock` or `timeout` is not on `PATH`, when `DBUS_SESSION_BUS_ADDRESS` is unset, when `lib/model-gateway-url.sh` refuses `LEGION_E2E_MODEL_GATEWAY_URL`, when the keyring is locked (`the operator's keyring is locked, so hawk-token cannot read the hawk login: unlock it (the
unlock-keyring skill) and rerun`), when the key command gets no key (quoting its own reason:
the outcome and `hawk-token`'s last stderr line), and when `hawk-token` prints anything but one JWT; an argument refusal exits 2.
The installer runs that one call with `--preflight`, which exempts it from the key command's deadline (below):
on a machine where `hawk-token` has never run, its first-run build takes about a minute in the foreground, and it happens here rather than inside a pane's ten-second `!command`.
The key is never printed.

The key command mints once and keeps the key in `<cache-dir>/hawk-token.key` (`0600`) until 300 seconds before its JWT `exp`, or for 300 seconds when the key has none.
Each `hawk-token` run reads the hawk login from the keyring over the session bus, and a host's keyring daemon died serving such a read at 09:33Z on 2026-09-24, relocking the keyring mid-run.
A whole tree's run on tmux invoked the command 29 times, once per pane launch plus the preflight, and each was a mint before the cache.
Every call inside the window gets the kept key.
A key the gateway refuses before then is not re-minted: the run's model turns fail, loudly.
(A call's parent is a helper the Oh My Pi process starts for each call, on 18.2.9 a child of the agent's own `omp`, so the record names the agent by the Legion pane's role and generation instead.)

One call mints at a time: a wave of agents that starts as the kept key expires calls the command at once, and concurrent mints on a loaded host run past `hawk-token`'s 9000 ms budget.
A call that finds no kept key takes an `flock` on `<cache-dir>/hawk-token.key.lock`, looks at the cache again, and mints only when the key is still missing;
every other call waits on the lock and serves the key the holder kept.
The mint runs with the lock's descriptor closed, since `hawk-token` can start a detached refresh that would otherwise hold the lock for minutes.

Oh My Pi kills a `!command` 10 s after it starts it, so every call but the preflight gives up at 9500 ms of a clock that starts with its own process (under load bash can take half a second to reach its first line), whether it is waiting on the lock or minting under `timeout`, and logs why.
A waiter that takes the lock with under 2000 ms left starts no mint:
the fastest mint measured took 2006 ms, and each attempt is another keyring read.
A waiter's wait is bounded by one mint, since it started no earlier than the call it waits on, so a wave served by a mint that succeeds is served inside every caller's ten seconds.

Every agent's call appends one tab-separated line to `<dir>/hawk-token.calls`, and to the caller's `MODEL_GATEWAY_CALLS_FILE` when its environment names one:
the time, the caller's pid, the directory Oh My Pi ran it in, the agent, the outcome, and a detail.
The fields, and the values the agent and outcome take, are stated once, in the header of [`lib/model-gateway-unserved.sh`](#libmodel-gateway-unservedsh), the file that reads them.
The installer's preflight is no agent's and is left out;
a call that gets no key also says why on stderr, which is where the installer reads the preflight's reason.
The key command tells a spent budget from a refused login only by `hawk-token`'s own sentence (`in <spent> ms of a <budget> ms
budget`, spent at least the budget), since the wrapper exits 1 either way;
a `hawk-token` that words it otherwise has a spent budget recorded as `failed`, never as a starve.
A harness that gives each agent its own file can judge one run from that run's directory alone.

The script creates the profile's two files, `<dir>` and `<cache-dir>`, and removes none of them; the caller does, with its work directory and `<cache-dir>`.

## lib/install-key-file-model-route.sh

Routes a named OMP profile's model turns straight to a model endpoint, Anthropic's own unless `--base-url` names another, keyed from a local file:
the key-file alternative to [`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh)'s Hawk route, for a machine that has no Hawk gateway at all.
Mirrors the model-route pattern [`deploy/kubernetes/operator-route/models-key-file.yml`](../../deploy/kubernetes/operator-route/models-key-file.yml) gives a deployment's pods:
an operator-supplied key, read fresh on every call, never minted and never written into the tree.

```sh
scripts/e2e/lib/install-key-file-model-route.sh --profile <name> --home <dir> --dest <dir> --key-dir <dir> --key-file <file> [--base-url <url>]
# → <key-dir>/anthropic-key
```

`--base-url` (default `https://api.anthropic.com`, so a caller that names none gets Anthropic's own endpoint) is the `anthropic` provider's `baseUrl`:
an Anthropic-compatible model gateway (a LiteLLM proxy, say) whose gateway key `--key-file` holds, or a proof's own model stand-in, as [`stage2-tmux-supervision.sh`](#stage2-tmux-supervisionsh) names [`lib/messages-standin.ts`](lib/messages-standin.ts)'s.
It must be an `http://` or `https://` URL of letters, digits and `:/._~-` alone, so no user or password, ending in neither a colon nor a slash, refused otherwise ([`lib/model-endpoint.sh`](#libmodel-endpointsh)'s `model_base_url_refusal`).

`--key-file` names a file holding the plaintext key that endpoint takes, alone (one line;
a trailing newline is read past), refused unless it is a regular file only its owner can read (no group or other permission bit)
— checked once, here, before any pane exists, never read beyond that check, so a key a caller rotates mid-run takes effect on the very next call with no restart.

`--dest` and `--key-dir` must be different directories (refused otherwise), and the caller gives them different lifetimes.
`--dest` gets only the call record (`<dest>/anthropic-key.log`, `<dest>/anthropic-key.calls`), which carries no secret and is what a stage proof keeps as evidence after every outcome.
`--key-dir` gets the key command itself (`<key-dir>/anthropic-key`):
a plain file read, with `--key-file`'s resolved path baked into it, so anyone holding that script can read the key for as long as `--key-file`'s path still holds one
— the caller must give `--key-dir` a path its own cleanup removes unconditionally, on every exit, not the evidence directory or a workspace a failing run keeps for debugging, the same rule [`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh)'s `--cache-dir` already follows for its own minted token file.
Unlike `hawk-token` there is nothing to mint and no login to lose to a relocked keyring, so there is no cache and no budget:
every call is a plain, fast file read, `served` or `failed` (there is no `timeout` or `killed` here, since nothing is minted or waited on).
It appends one tab-separated line per call to `<dest>/anthropic-key.calls`, and to the caller's `MODEL_GATEWAY_CALLS_FILE` when its environment names one, in exactly the record shape [`lib/model-gateway-unserved.sh`](#libmodel-gateway-unservedsh) reads
— the installer's preflight (`--preflight`) is no agent's and is left out, the same rule `hawk-token`'s own key command follows.
The profile's `agent/models.yml` points the `anthropic` provider at `--base-url` with `apiKey` and `X-Api-Key` both `!exec <key-dir>/anthropic-key`, and its `agent/config.yml` pins every model role to `anthropic/claude-opus-4-8` and disables every other provider a pane could reach without the key file, the same shape [`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) writes, `startup.setupWizard: false` included.

The script creates the profile's two directories, `<dest>` and `<key-dir>`, and removes neither;
the caller does, with its work directory, `<key-dir>` unconditionally and `<dest>` only when it keeps no evidence.

## lib/model-endpoint.sh

The model endpoint a key file's key is for, sourced by `clean-session-stop-tmux.sh`, whose own `note` and `fail` it calls, by [`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh), and by [`nats-callout-sessions.sh`](#nats-callout-sessionssh), which also holds Codex's route to its key with `responses_key_preflight`.
`anthropic_base_url` is Anthropic's own endpoint, the one a run with no `LEGION_E2E_MODEL_BASE_URL` calls.
`model_base_url_refusal URL` prints why `URL` cannot be a model base URL, and nothing when it can:
an `http://` or `https://` URL of letters, digits and `:/._~-` alone (so it carries no user or password, and stays a plain YAML scalar), ending in neither a colon nor a slash.

`model_key_preflight BASE KEY HEADER WHAT` holds the endpoint `BASE` (Anthropic's own when empty) to the key in `KEY`, the run's 0600 copy of the key file `WHAT` names:
`GET /v1/models` with the key must answer 200, or the run fails naming the endpoint and the code.
With `BASE` named it also asks `GET /v1/models` with no key, which must not answer 200 (an endpoint that serves a caller with no key proves nothing about the key;
the code a refusal gave is noted), and `GET /key/info` with the key (a LiteLLM proxy serves key management at its root, not under `/v1`), whose `key_alias` (the proxy's non-secret name for the key, at the top level or under `info`) is noted as `the model key is <alias>`, or that the endpoint names no alias, never failing on it.
Every call sends `anthropic-version: 2023-06-01`;
the key reaches curl in `HEADER`, a 0600 header file removed before it returns (the callers' cleanup removes it too, after a `fail`), never an argv, and neither the key nor a response body is printed.

`responses_key_preflight BASE KEY HEADER WHAT MODEL` holds an OpenAI Responses-compatible endpoint `BASE` (a Codex model provider's `base_url`, its `/v1` included) to the key in `KEY` the same way, sent as a bearer:
`GET /models` with the key must answer 200 and list `MODEL`, and `GET /models` with no key must not answer 200.

## lib/codex-host.sh

The Codex CLI sessions of [`nats-callout-sessions.sh`](#nats-callout-sessionssh), which sources it; it keeps the Codex sessions' lifecycle, and the script keeps the checks and the one table of how each session is told its credential and what each control changes (`told`, `withholds`, `manifest_drops`), which it reads.
`codex_pack` builds and packs the Codex plugin as the release does (`scripts/build.ts --codex-out`, `bun pm pack`) into a marketplace of the run's own, takes the control's variables out of its manifest's `env_vars`, and in the default mode compiles the stand-in `codex` (`packages/pi-envoy/scripts/role-takeover/codex-client.ts`).
`codex_start` starts each Codex session and makes its first prompt: under the stand-in in the default mode, and as the pinned Codex CLI in a tmux server of its own with `--real-hosts`.
`codex_stop` keeps each real Codex's terminal in the evidence and quits it, and `codex_clean` keeps the stand-in transcripts and removes the real sessions' Codex homes and sockets.
Sourced, never run: it defines functions and starts nothing of its own.

## lib/model-gateway-unserved.sh

Says whether the key command [`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) or [`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh) wrote left an agent without a key, and why.
Oh My Pi answers a call that gets no key in one of two ways, and neither says why the key was missing, so the run itself cannot say why it never got a model turn.
A provider with no key at all gets `No API key found for anthropic.` and exit 1 (and for `omp -p` on 18.2.9 no session file at all).
A key command (`!exec …`, as both installers write it) that prints nothing is run again three times, about 30 s apart, and the call then exits 1 after about 105 s with `The apiKey command for provider anthropic produced no key`: measured on 2026-11-19 on `18.18.6-sami.20261119-162325` and on `18.18.6-sami.20261117-013345`, which behave the same.

```sh
bash scripts/e2e/lib/model-gateway-unserved.sh --record "$run/model-gateway-calls"                                # a scorer: one agent run
bash scripts/e2e/lib/model-gateway-unserved.sh --notes "$evidence/model-gateway" "$check_started" "$check"  # a failed stage proof's EXIT trap
```

An agent is a working directory and an agent field together, so a relaunched pane, with its new generation, is a new agent.
An agent's last call decides whether it went without a key: Oh My Pi retries a failed key command 30 s later and after a 401.

`--record` scores one agent run from the `MODEL_GATEWAY_CALLS_FILE` a harness named in that agent's environment:
a file of the run's own, which does not exist before the run, so every line in it is that agent's.
The skill-scenario rig runs one `omp -p` per run, which keeps its key for the life of the process, so a starved run has no model turn and the record agrees with the outcome.
It exits 0 when the last call was served or the file holds none (no file included), `75` (`STARVED, not scored`:
rerun it) when that call ran out of time or had its mint killed, and `77` (`KEY FAILED, not scored`) when the mint failed otherwise, which a rerun does not fix.
A run from a checkout whose key command predates the record carries the same signal without the reason: no session file at all.

`--notes` is a stage proof's diagnostic:
the `EXIT` trap runs it once it has decided the run failed, guarded so that it never changes the exit status, and not after a hangup, an interrupt or a termination (129, 130, 143), which stopped the run rather than failed it;
a child a signal killed is a failure and still gets them.
It prints, after the check's own failure, each agent that could have failed the check for want of a key: when it last got none, from where, and why.

- **An agent whose last call got no key is still without one.**
  It is listed whenever that call came, marked when it came before the check began: it had no key when the check asked it.

- **An agent served again after its last starve recovered.**
  It is listed, with when it was first served again, when that starve came at or after `<since>` less 30 s.
  The 30 s is the wait Oh My Pi keeps before it runs a failed key command again (`COMMAND_FAILURE_RETRY_MS`, 30 s in the Oh My Pi `.omp-pin` names):
  a request inside it fails with no key and runs no command, so it leaves no line, and a check's first calls can fail on a starve from just before the check began.

- **An agent served again before that window** held a key through the whole check, and is left out.

A person then sees whether starvation could explain the failure.
A run-wide "not scored" would not be honest:
where the key command's one caller is the operator's controller while every pod uses its projected token, a worker's failure cannot come from a starved controller.

Every form exits 2 on an argument refusal (a `--record` in a directory that does not exist included), and `--record` and `--notes` 1 on a record line they cannot read.

## lib/check-model-route.sh

Proves a tmux stage proof's agents reached the model only through its configured route:
every agent turn the isolated OMP profile recorded, each subagent's included, was served by the `anthropic` provider, the one [`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) (the Hawk gateway) or [`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh) (a local key file) routes and leaves enabled.
Stage 2 runs it last, after `stop`.

```sh
bash scripts/e2e/lib/check-model-route.sh --sessions "$work/omp-home/.omp/profiles/<profile>/agent/sessions" --control "$work/model-route-control"
# → 74 assistant turns in 5 agent sessions (0 of them subagents'), every one on the anthropic provider; negative control: … refused
```

Each session is a JSONL file under `--sessions`; a subagent's is the `<AgentName>.jsonl` in its parent session's own directory.
Every assistant turn must record `message.provider` `anthropic`, and every `model_change` a model under `anthropic/`;
a line that does not parse is skipped, since a live agent may be mid-write.
It exits 1 naming each session off the route with what it recorded, and when no session, or no assistant turn, exists.
Run over the transcripts of a whole tree's run on tmux from before either model route, it names the retro scout's Bedrock turn:

```text
check-model-route: agent sessions off the anthropic route:
…/RetroFreshEyes.jsonl: model change to amazon-bedrock/openai.gpt-oss-120b
…/RetroFreshEyes.jsonl: turn on amazon-bedrock/openai.gpt-oss-120b
```

Every run also carries its own negative control:
the first session holding a turn is copied into `--control` with that one turn rewritten as `amazon-bedrock/us.anthropic.claude-opus-4-8`, and the same check must refuse the copy, or the run exits 1 (`the negative control passed`).
An argument refusal exits 2.

## lib/transcript.sh

A script's transcript.
Stage 2 and `controller-start-tmux.sh` source it before their first output, and `clean-session-stop-tmux.sh` and `dispatch-session-settings.sh` source it too.

```sh
. "$root/scripts/e2e/lib/transcript.sh"     # sourced, never run
transcript_to "$evidence/transcript.log"
```

`transcript_to FILE` sends the calling shell's stdout and stderr to `FILE` as well as to whatever read stdout before.
It does this through a `tee` that ignores the signals a driver traps and SIGPIPE.
It opens `FILE` for append in the calling shell first, and returns that open's failure, which names `FILE`, before it moves either stream:
the `tee` opens `FILE` in a process of its own, whose failure no caller sees, so a run under `set -e` that could not open its transcript would otherwise go on to its PASS with none.

- A signal to the process group, a reader that goes first (a supervised launcher's own `tee`, stopped with the run), or a full disk under `FILE` each cost `tee` at most the outputs they break.
  The driver and its cleanup never fail on a write.
  Without it, a group TERM after the reader had gone would end a driver with a TERM trap before its `EXIT` trap ran:
  bash writes its `Terminated` notice for the interrupted command to the dead pipe first.

- GNU `tee` stops once every output has failed and never reopens one.
  `/dev/null`, an output that never fails, is what keeps it draining once both the reader and `FILE` have failed.

- busybox `tee` keeps writing every output and reports errors at EOF, so it picks `FILE` up again once its disk has room.
  It rejects `-p`, which is why the trap, not `-p`, carries SIGPIPE.

- The `tee` must outlive the run's sweeps, which end every process that carries the run's mark ([`marked_processes`](#libprocess-treesh)).
  A caller marks its run (`export LEGION_E2E_RUN=$work`) after `transcript_to`, so the `tee`, which takes the caller's environment as it stands at the call, carries no mark, whatever `FILE` and the caller's working directory are.

`lib/transcript.test.ts` (`bun test scripts/e2e/lib`) sources the helper under `set -euo pipefail` and runs two lines after it, one on standard output and one on standard error:
a file it can open takes both, which still reach the reader, and a file whose directory does not exist, or a directory in its place, ends the run before its line on standard output, naming the file.

## lib/rig.sh

The host-side helpers a script sources for its rig: bounded waits, ports, the processes the run starts, and their teardown.
Stage 1, `controller-start-tmux.sh`, `clean-session-stop-tmux.sh`, `dispatch-user-turns.sh`, `dispatch-session-settings.sh`, `dispatch-claimant-delivery.sh` and `verification-results.sh` source it, as do the docs site's broker and operator-walkthrough recorders and pi-legion's skill-scenario rig.
Six of the eight scripts that call [`lib/loopback-dsn.sh`](#libloopback-dsnsh)'s helpers source it too, for those two helpers, which this file sources for them.
`stage2-tmux-supervision.sh` sources `lib/loopback-dsn.sh` on its own, and `envoy-session-delivery.sh` sources it and [`lib/port-file.sh`](#libport-filesh) on its own and keeps an `await_port_file` and a `keep_port` of its own.

```sh
. "$root/scripts/e2e/lib/rig.sh"     # sourced, never run
```

The caller sets `root` (the checkout), `work` (the run's scratch directory), `evidence` (each service started by `start_process` logs to `$evidence/logs/<name>.log`) and `timeout_hook` (empty, or a function a timed-out wait runs before it fails), and defines `note` and `fail`, which exits.
A caller that sweeps, or checks that no process of its run remains, also exports `LEGION_E2E_RUN=$work` once its transcript runs, and names it in every start that clears its environment: the processes that carry that mark, or a daemon pane's `XDG_CONFIG_HOME` under `$work`, are the run's ([`marked_processes`](#libprocess-treesh)), and no process that only works under `$work` or names it on its command line is.
Stage 1 calls `real_bin_path`, `keep_port`, `start_kept` and `held_only_by`.

Each item below names a function, then what it does.

- `until_true SECONDS WHAT CMD…`

  runs CMD every half second until it succeeds.
  After SECONDS of wall time, each poll's own run included, it runs `timeout_hook` and fails naming WHAT.
  It says what it waits for on entry and every 60 s.
  A poll that never returns would hold the wait, so callers bound their remote calls.
  A guard that writes `$evidence/pane-endpoint-violation.txt` makes the next wait abort, naming the violation

- `start_process NAME CMD…`

  starts CMD in the background, appending to NAME's log, and sets `NAME_pid`, the leader of a process group of its own (monitor mode), with its start recorded (`record_start`).
  A service that has already exited when its start is read, or is a zombie, is recorded `gone`, so `stop_pid` and `signal_group` send it nothing and say nothing, and `start_process` still succeeds, so a driver's `set -e` never ends the run there with no line saying why: `await_start` names the service's log.
  A `setsid` in front of CMD would fork, since the job already leads its group, and leave `NAME_pid` a `setsid` that exits at once; `lib/unread-proc.test.ts` holds both, and that the broker walkthrough rig (`docs/site/media/broker/rig.sh`) starts no service through one

- `start_held NAME PORT_VAR FD_ENV [--env K=V]… -- CMD…`

  starts CMD as the service NAME through [`lib/held-port.py`](#libheld-portpy) `exec`:
  a socket listening on a port the kernel chose, held from that choice until CMD serves on it, handed over as descriptor 3 with `FD_ENV=3`;
  `{port}` in an `--env` value or in CMD is its port.
  It waits, bounded, for the helper's port file and sets PORT_VAR before CMD runs; `NAME_pid` is CMD's own pid.
  A CMD that clears its environment (`env -i`) names `FD_ENV=3` itself

- `keep_port NAME PORT_VAR`

  starts the keeper NAME (`held-port.py keep`, its socket `$work/NAME.sock`), which holds one socket on a port the kernel chose for the whole run, and sets PORT_VAR once it hands out copies.
  The caller's cleanup stops it (`NAME_pid`).
  A configuration can name the port before its service starts, and a restart cannot lose it.
  Between two servers the port listens unanswered, so every poll of it carries a timeout

- `start_kept NAME KEEPER FD_ENV [--env K=V]… -- CMD…`

  starts CMD as `start_held` does, on a copy of the socket the keeper KEEPER holds (`exec --from`)

- `report_port LOG PATTERN`

  prints the port a process that bound port 0 itself reported:
  the digits right after the first match of the extended regex PATTERN in LOG (`'Listening for client connections on [^ ]*:'` for `nats-server -p -1`).
  It fails while no line matches, so it is also the poll that waits for one

- `port_answers PORT`

  succeeds when something accepts a connection on `127.0.0.1:PORT`, through bash's own `/dev/tcp`.
  It is how a proof refuses a port of its own that is already answering, before it starts anything.
  It asks a different question from `listening_holders` below, which reads who holds a socket that is already listening

- `listening_holders PORT`

  prints the pid of every process holding the one socket listening on PORT:
  its inode from the `LISTEN` row of `/proc/net/tcp` and `/proc/net/tcp6`, its holders from `/proc/*/fd`.
  The shell's glob hands every `/proc/<pid>/fd` to `find` on stdin, never as its arguments, which past ARG_MAX (about 80,000 processes) could not start, and the empty answer read as no holder.
  An fd directory this user may not read, or whose process ended during the walk, is none of the answer; any other complaint of the walk's prints and fails with 2.
  It fails when no socket, or more than one, listens on PORT

- `held_only_by PORT PID…`

  succeeds when the holders of the socket listening on PORT are exactly the PIDs given; otherwise it prints each holder's pid and command line and fails.
  Stage 1 runs it after each daemon start with the daemon and its keeper

- `await_start NAME PID SECONDS WHAT CMD…`

  waits, bounded, for CMD to succeed while PID lives, and fails naming NAME's log the moment PID exits

- `stop_pid PID` (in [`lib/process-tree.sh`](#libprocess-treesh), which this rig sources)

  TERM, then KILL after 10 s, each sent through `freeze_if` held to the start time recorded for PID when the run learned it (`record_start`; `start_process` records each service, and a script records each `$!` and pid file it reads), so a pid whose process ended and passed to another process since, another user's or another run's, gets no signal; best effort, so a failed cleanup never hides the check that failed.
  An empty PID does nothing, a PID with no recorded start is named and left alone, and a PID that cannot name one process of the run (0 however spelled, so any value with a leading zero, 1, a negative or non-numeric value, the calling shell or its process group's leader) is refused with a printed line ([`signalable`](#libprocess-treesh)), since `kill 0` signals the caller's whole process group

- `run_processes`

  prints the pid of every process of the run still alive: each that carries the run's mark ([`marked_processes`](#libprocess-treesh)), which the run exports as `LEGION_E2E_RUN=$work` right after its transcript starts, or a daemon pane's `XDG_CONFIG_HOME` under `$work`.
  A process that only works under `$work` or names it on its command line carries neither, so a person's shell `cd`'d there, a `tail -f` of the transcript, an editor on a file under it, or another agent's `grep -r` over it is none of the run's.
  It prints why and exits 2 when the run's processes could not be read, which the cleanup checkpoint's wait and the two stage proofs' last checks fail on.
  Each sweep that ends them is `stop_marked_processes "$work"`, which signals only the marked processes

- `real_bin_path DEST NAME…`

  symlinks `DEST/NAME` to the real `NAME` binary `mise which NAME` resolves in the caller's own environment, for every `NAME`, and prints `DEST`:
  a `jj` that mise shims resolves through a shim chain that a pane's relocated `HOME` (`make_omp_home`) breaks, since mise has no pin for a directory whose global config a relocated `HOME` hides
  — `jj`'s own shim hands the call straight back to itself, exit 127.
  tmux also breaks for the daemon itself, which runs it with the pane environment's allow-list alone:
  under the daemon's tmux environment, which moves the XDG directories under the state directory (a relocated `HOME` has the same effect), a shimmed tmux run from a directory under the operator's real `HOME` finds that `HOME`'s mise config as a parent directory's config with no trust record, and exits 1, which the daemon's boot reads as a failed scrub of its private server's environment, not as no server, so boot refuses.
  Every `NAME` must resolve through the caller's own mise configuration; it refuses naming the tool if `mise which` fails.
  Stage 1 prepends `real_bin_path "$work/realbin" tmux` to its daemon's PATH when its first tmux is a script or mise itself, and keeps a real tmux binary, such as CI's apt tmux, otherwise;
  Stage 2 and the Claude Code backend's test on tmux, which do not call it, put a link to `mise which tmux` on their daemons' PATH the same way

`lib/run-processes.test.ts` (`bun test scripts/e2e/lib`) runs `run_processes` over its host's own `/proc`, under an `ERR` trap that prints, with a `readlink` on `PATH` that records every call and answers nothing and a `grep` that counts its calls:
it names a process that carries the run's mark and a pane-shaped one whose `XDG_CONFIG_HOME` is under the run's state, leaves a process working in the run's directory, a `tail -f` of its transcript and an unrelated one alone, runs a handful of greps and no `readlink`, and trips no trap.
It names a marked process double-forked to pid 1, which no tree the run started holds.
Under a 512 KiB stack (ARG_MAX 128 KiB) and an environment measured to fill it ([`lib/arg-max-force.sh`](#libarg-max-forcesh)), where `find /proc/[0-9]*/cwd` fails with E2BIG, it still names the run's process, and `listening_holders` still names the test process holding a listening socket; the forms they replaced name none.
Where the kernel does not hold an exec to ARG_MAX, as gVisor does not, those two rows are skipped by that name (`argMaxForceHolds`, [`lib/arg-max-force.sh`](#libarg-max-forcesh)).
A `find` or a `grep` that a signal ends after part of its output makes `listening_holders` and `run_processes` exit 2, a run that has not marked itself has no processes to read, and an empty run directory names none (`marked_processes`, `procs_naming` and `names_dir` exit 2).
A marked process whose name holds a newline and a `) ` is still named and ended.
`stop_marked_processes` ends the marked processes, an escaped one too, and leaves a shell and a `tail -f` under the run's directory running, not stopped;
with a grace it gives each SIGTERM and the grace, then SIGKILLs only a survivor;
it signals no pid whose start time is not the one its walk read;
and it ends a child a marked process started under `env -i`, which carries no mark, with or without a grace, also when the grace's SIGTERM ends the process that started it.
Every process a row signals, and every harness shell, runs in a process group of its own ([`lib/own-group.ts`](#libown-groupts)).

## lib/own-group.ts

`ownGroup(argv)` is `argv` run as the leader of a process group of its own inside the test runner's session, with the runner as its parent: a `perl` that calls `setpgrp(0, 0)` (setpgid) and then execs `argv`, so the pid `Bun.spawn` returns is `argv`'s own.
perl changes nothing else `argv` gets: no signal disposition and no environment variable.
A `python3` in its place ignored SIGPIPE and SIGXFSZ and, in a C locale, coerced `LC_CTYPE`, and its exec handed all three on, so a row ran with SIGPIPE ignored where the rigs do not.
A row that stops a process (SIGSTOP, through `stop_tree`, `freeze_if` or a sweep of [`lib/process-tree.sh`](#libprocess-treesh)) must not stop a member of the runner's own group: where that group is orphaned, as in a Legion pod, whose bash tool starts each command as the leader of a session of its own, gVisor sends SIGHUP and SIGCONT to an orphaned group with a stopped member whenever another member exits, and `bun test` dies of that SIGHUP without naming a row.
A Legion pod (`4.19.0-gvisor`) still did so when measured on 2026-11-18.
gVisor's commit 553528ee1034 (2026-10-07) sends them only when the group becomes orphaned, as Linux does: under runsc release-20260928.0 a stopped member of an orphaned group got SIGHUP when another member exited, and under release-20261012.0 and release-20261116.0 it did not.
A group whose parent, the runner, is in another group of the same session is not orphaned while the runner runs.
A session of its own (`setsid`, or `Bun.spawn`'s `detached`) is the wrong tool: its group is orphaned from the start.
`untilRunning(pid, program)` resolves once `pid` runs `program` (its `/proc/<pid>/exe`), and fails naming both and what the pid ran instead when it does not within 10 s: a sweep row waits on it, since a sweep that stopped the perl before its `setpgrp` would stop a member of the runner's own group.
`lib/own-group.test.ts` holds that a wrapped shell leads its own group under the runner and has the same traps, blocked and ignored signals and environment as a plain one, and that `untilRunning` fails at its bound naming the pid and the program.
It reads the ignored signals through the shell's own `trap -p` of each signal, and the blocked set through a perl the shell starts (`sigprocmask`; perl itself ignores SIGFPE, so its `%SIG` could not tell one it was handed), not `/proc/<pid>/status`, which a Legion pod's kernel writes no `Sig*` line to, and it fails where either shell's read prints no line.
`lib/run-processes.test.ts`, `lib/process-tree.test.ts`, `lib/controller-start-guards.test.ts`, the skill-scenario rig's sweep rows and `packages/pi-legion/scripts/skill-scenarios/rig-isolation.test.ts` use `ownGroup`, and skip by name where the host has no perl

## lib/arg-max-force.sh

`arg_max_force ENTRY ROOM SCRATCH OLD_FORM` puts a test's shell under the force where a row proves that a walk of every process hands the proc root's `<pid>/ENTRY` entries to no program as its arguments.
The walk reads `proc_root`, a proc root of the row's own processes ([`lib/proc-root.ts`](#libproc-rootts)), never the host's `/proc`, so the row's time is its own.
A 512 KiB stack makes ARG_MAX 128 KiB, and an exported `PADDING` fills the environment to ROOM bytes short of the largest that still lets a program of no arguments start, measured there and then.
An exec handed every entry as an argument then fails with E2BIG, as one does unforced at about 80,000 processes, while an exec of a few arguments still starts.
A root whose entries together are under twice ROOM gets more first: entries of its own at pids above the kernel's `pid_max`, which no process can hold, each an empty `environ` and empty `fd` and `cwd` directories, counted in the shell and made with one `mkdir`; each padding of the measurement is built in the shell too (`printf -v`), so the force starts the same few programs however many entries it needs.
The control it must pass runs there too: OLD_FORM, the command line the walk replaced, fails with E2BIG, and a program of no arguments still starts; a force that is not in force, or a shell with no `proc_root`, exits naming why.
`lib/run-processes.test.ts` and the skill-scenario rig's `run_env_pids` row (`packages/pi-legion/scripts/skill-scenarios/rig.test.ts`) source it.
`lib/arg-max-force.ts`'s `argMaxForceHolds()` says whether the kernel holds that exec at all: an environment of two 100,000-byte variables under a 512 KiB stack must stop a program of no arguments, as Linux does; gVisor starts it, so there the force cannot be in force, and those rows are skipped by that name rather than failed or passed

## lib/proc-root.ts

A proc root of a test's own processes, for code under test that reads `${proc_root:-/proc}`: a directory holding a link to the real `/proc/<pid>` of each process the test names (`procRootOf`), so that code reads those processes' own files and signals their real pids, and its work is the test's processes rather than the host's.
The walks of every process read it: `procs_naming` (and so `run_processes`, `marked_processes`, the sweep and the skill-scenario rig's `run_env_pids`), `listening_holders`, `refuse_leftovers` and `lib/role-namespaces.sh`'s `role_kill_sh`.
A walk of the host's whole `/proc` held a row to the host's process count; on this devbox at 25,474 entries `refuse_leftovers` alone took 29 s.
Each row of `lib/run-processes.test.ts` walks a root of the processes it started and the test process, and its first row starts a marked process outside that root, which a walk that went back to the host's `/proc` would name.
A harness shell makes the same root for itself with one `ln -s` an entry

## lib/process-tree.sh

How a live proof knows a process it started: by the pid it had when it started, read from what started it, and by the program the kernel records it runs (`/proc/<pid>/exe`), never by a walk of the machine's `/proc` for its name, its command line or its working directory.
On a devbox at load 469 one such walk of 29,613 entries took 504 s; every other Oh My Pi there runs under the same name, its own daemon broker child among them; an `omp_invocation`, a wrapper or `exec -a` gives `argv[0]` any name;
and an entry read through `tr | grep -q` under `pipefail` reads as absent whenever grep stops before tr has written the rest, which missed 2 to 4 reads in 100 of a devbox's own environment and every read of one with a mebibyte after the variable.
[`lib/rig.sh`](#librigsh) sources it, [`lib/leftovers.sh`](#libleftoverssh) sources it when no script has, and `stage2-tmux-supervision.sh`, which sources no rig, sources it on its own.
It needs nothing of its caller's.
Its walks of every process (`procs_naming`) read `${proc_root:-/proc}` ([`lib/proc-root.ts`](#libproc-rootts)) and hand the entries to `grep` through `xargs` in calls that fit ARG_MAX, and [`lib/rig.sh`](#librigsh)'s `listening_holders` and [`lib/leftovers.sh`](#libleftoverssh)'s `refuse_leftovers` hand them to `find` on stdin (`-files0-from -`), which needs GNU findutils 4.9 or later: on an older `find` (4.8, Ubuntu 22.04's) both always exit 2.
Both read their `find`'s stderr with `proc_walk_complaint ERRFILE PROC`, which prints its first line that is neither an entry of PROC this user may not read nor one whose process ended during the walk, and fails when there is none.
A walk or a read that cannot see the processes it reads says why and fails with 2, never reads as none: a proc root that lists no process, an `xargs` or a `find` a signal ended, an unreadable `/proc/net/tcp`, and a Linux `/proc` with no `self` (an empty tmpfs mounted there, `proc_unreadable`), where `proc_children`, `proc_descendants`, `stop_tree`, `kill_frozen`, `kill_held` and the sweep fail with 2.

```sh
. "$root/scripts/e2e/lib/process-tree.sh"     # sourced, never run
```

- `proc_parent PID`, `proc_group PID`, `proc_children PID`

  print PID's parent, its process group, and each of its children, one a line, from that process's own `/proc/PID/stat` (read whole and past its last `) `, since a command name may hold both and a newline) and every thread's `/proc/PID/task/*/children`.
  None reads every process of the machine, as procps-ng's `ps -p` and `pgrep -P` do: one `ps -o args= -p PID` opened `/proc/<pid>/stat` 12,564 times at about 13,000 processes.
  Each prints nothing, and succeeds, for a process that is gone, so a caller under `set -e` may assign its answer bare; a host with no `/proc` asks `ps` and `pgrep`, and `lib/process-tree.test.ts` holds the `ps` path to the same answers.
  [`signalable`](#libprocess-treesh), `stop_tree` (below), `stop_pid` (below) and the skill-scenario rig's `stop_env_processes` use them.
  `proc_children` asks `pgrep` as well where a process's `task/<tid>/children` file is missing (a kernel built without `CONFIG_PROC_CHILDREN`), so `stop_tree` never leaves the children of such a process running, and fails with 2 on a `pgrep` that exits above 1 and on a `/proc` that cannot be read

- `proc_start PID`

  prints PID's start time, field 22 of `/proc/PID/stat`, which the kernel sets at the fork and exec keeps, and nothing for a process that is gone, a zombie included:
  a pid and its start name one process even once the kernel has given the pid to another

- `record_start PID`, `record_pid_file FILE VAR`, `stop_pid PID`, `signal_group PID SIGNAL`

  `record_start` records PID's start time in `started_at`, read where the run learns the pid (right after its `&`, or from its own pid file), and fails for a pid with no live process.
  `record_pid_file` reads the pid and start a process wrote of itself before it ran (`record_pid_sh`) into VAR and `started_at`, so a read made late, in a cleanup after the process ended, still holds the pid to that process's start; the broker walkthrough rig reads its helper's pid with it.
  `stop_pid` sends TERM, then KILL after 10 s, and `signal_group` sends SIGNAL to the group PID leads, each through `freeze_if` held to that recorded start, so a pid whose process ended and passed to another since gets no signal; a pid with no recorded start is named and left alone, and one recorded `gone`, whose process ended before its start was read, is left alone with no word.
  A leader that has gone takes its group's signal with it: a `signal_group` after `stop_pid` of the same leader sends nothing.
  A member that outlived its leader is the run's sweep's (`stop_marked_processes`), which the clean-session-stop cleanup runs.
  `lib/unread-proc.test.ts` holds `stop_pid` and `signal_group` with a start that is not the process's own and with a pid recorded `gone`, and, where `sudo -n` can make a pid namespace whose `pid_max` is 2400, `stop_pid` with a daemon that ended and a stranger that took its pid.
  That row runs only on Linux 6.14 or later, where a pid namespace's `pid_max` is its own: on an older kernel the write would set the host's, so there it is skipped by that name; and the row fails if the host's `pid_max` ever moves across it, naming the value it read, the restore's exit status and stderr, and the value it reads after the restore

- `signalable PID`

  is whether PID can name one process the run started, refusing with a printed line 0 however spelled, 1, a negative or non-numeric value, the calling shell and its process group's leader

- `stop_tree PID [PARENT]`, `proc_descendants PID [START]`, `kill_frozen PID`, `kill_held DIR NAME…`

  `stop_tree` stops a background loop with every process under it:
  it freezes the loop, stops each child the same way, then kills the loop, so the `kubectl`, `jq` or `sleep` the loop was waiting on goes with it.
  It signals a pid only while its parent is PARENT (default the calling shell), so a pid the watcher left and another process reused is never hit, and refuses the same pids `stop_pid` does:
  `kill -STOP 0` would freeze the whole driver and a later `kill -KILL 0` end it.
  It and the refusal read only the `/proc` entries of the processes they ask about (`proc_parent`, `proc_group` and `proc_children`), never every process of the machine as `ps -p` and `pgrep -P` do.
  `proc_descendants` prints `PID START` for every process below PID, each parent held to its process and not to its pid: with START, the parent's start must read START before its children are read and again after each child's parent check, and each child, taken only while its parent is that pid and its start reads the same before and after the check, has its own children walked held to its start; so a stranger that took a reaped pid under a running parent, and the stranger's children, are left out.
  `kill_frozen` ends a process a caller has stopped and held to its start and its mark (`freeze_if … STOP`) with every process under it, each child through `stop_tree` held to it as the parent, without reading its own parent again: a parent that exits after the freeze cannot leave it stopped and alive.
  `kill_held` reads `PID START` lines on stdin and ends each through `freeze_if … STOP` and `kill_frozen` while it is still that process and still carries one of the NAMEs under DIR.
  The run's sweep and the skill-scenario rig's `stop_env_processes` end their walks' processes with it, the one copy of that loop

- `incarnation_pid INCARNATION`

  prints the pid of a tmux claim's locator incarnation `<pid>:<start ticks>` while that pid is still the process that started at those ticks, and refuses a malformed incarnation, an ended process and a reused pid.
  Every rig that reads a pane from a claim's locator reads it through this, never through the pid before the colon alone

- `freeze_if PID START SIGNAL [CHECK…]`

  reads PID's start and runs `CHECK… PID` before it sends anything, and sends nothing to a pid that fails either; then it stops PID, reads both again, sends SIGNAL, and thaws it after any signal but KILL and STOP.
  On a mismatch at the second read it thaws PID only when its own SIGSTOP stopped it: a process that was stopped already stays stopped.
  A stopped process cannot end and hand its pid to another between the check and the signal

- `env_of PID NAME`, `env_has PID NAME`, `env_length PID NAME`, `process_args PID ARRAY`

  read PID's environment or command line whole, in the calling shell, with no pipe between the read and the check: `env_of` prints every value of NAME, `env_has` is 0 when NAME is there, 1 when it is not and 2 when the environment cannot be read, so an absence holds only on 1, and `env_length` prints the length of NAME's value (0 when absent), so a check of a secret never holds its value in a variable.
  Each reader of an environment turns xtrace off for itself, so a run under `bash -x` writes no value of it into its transcript

- `names_dir DIR NAME… PID`, `procs_naming DIR NAME…`

  are whether PID's environment gives one of the NAMEs the value DIR or a path under it, and `PID START` for every process whose environment does, read from every process of the machine (one `grep` per `xargs` batch, then each match read whole), failing with 2 when the walk could not read the machine.
  Both refuse an empty DIR with 2: `NAME=` followed by any absolute path would match it

- `marked_processes DIR`, `stop_marked_processes DIR [GRACE]`

  are a stage proof's run's own processes and its sweep: each process that carries `LEGION_E2E_RUN=DIR`, which the run exports after its transcript starts, or the `XDG_CONFIG_HOME` under DIR that a daemon pane carries, its environment built from the daemon's allow-list (`paneEnvAllowList`, `packages/daemon/internal/runtime/tmux/environment.go`).
  `marked_processes` refuses an empty DIR and a run that has not marked itself.
  `stop_marked_processes` stops each through `freeze_if`, so a pid that passed to another process since gets no signal, and then ends it with everything under it (`kill_frozen`), so a child that cleared its environment goes with it; with GRACE it gives each SIGTERM and GRACE seconds first, and what each had started, its children read while it was stopped for the TERM and theirs under running parents, each parent held to its start (`proc_descendants`), is ended by its own start time if it outlives the process that started it.
  Every stage proof's sweep is this one, and a start that clears its environment (`env -i`) names `LEGION_E2E_RUN` itself.
  A fork of the driver that execs nothing, a background subshell or function such as a `pane_watcher`, carries the driver's environment from the driver's own exec, without the mark, so no sweep sees it: each is stopped by the pid it had when it started (the list is in `run_marks`'s comment)

- `proc_describe PID…`

  prints one line for each PID that runs, with its parent, process group, session, start time (from `/proc/stat`'s `btime`) and command line, the fields of `ps -o pid,ppid,pgid,sid,lstart,args -p`, and nothing for one that has ended.

- `record_pid_sh`

  the POSIX sh a tmux pane's command runs as `sh -c "$record_pid_sh" record-pid FILE COMMAND…`:
  it writes its own pid and start time to FILE through a rename and execs COMMAND, so FILE names COMMAND's own process.
  The controller proof starts each `legion controller start` this way, and the Dispatch user-turn proof its session's Oh My Pi

- `recorded_pid FILE EXE`

  prints the pid FILE names while that very process runs EXE (its `/proc/<pid>/exe`): it fails before FILE is written, once the process has ended, and for a pid the kernel has since given to another process, of EXE or not, whose start time is not the one FILE holds

- `launched PID EXE…`

  prints the one process below PID that runs one of the EXEs, going below no process that does: below a pane, the agent past its `sh -c`, its `legion worker-shim` and the `mise x` that execs, whatever helper the shim runs beside it (agent-secrets' keygen and renew).
  It refuses none and more than one, naming them, and on none it names each process it passed and the program it runs.
  The controller, Stage 2 and clean-session-stop proofs find their agents with it

- `child_of PID`

  prints PID's one child, refusing none and more than one

- `child_running PID EXE`

  prints the one child of PID itself that runs EXE, refusing none and more than one:
  a pane's `legion worker-shim` is its process's child that runs the run's legion, and a Claude Code pane's `legion claude-bridge` is that shim's.
  It takes Claude Code as the bridge's one child once that child runs the program the run's `claude` ends in (its own path when it is the program itself, else the first `claude` on PATH that is, which a devbox's dotfiles wrapper hands the call to), never while it is the bridge's own fork or the interpreter of any wrapper on the way

- `native_program FILE`

  prints FILE's real path, and refuses a script (`#!`) and a mise shim: a script's process runs its interpreter and a mise shim execs the version mise picks, so no process runs either by the kernel's record.
  `launched`, `child_running` and `omp_program` take their programs through it.
  The clean-session-stop proof refuses an Oh My Pi named in `LEGION_E2E_OMP_PATH` that is either, and with it unset takes the first `omp` on PATH that is neither, as a devbox's own omp wrapper takes the first `omp` on PATH that is no wrapper

- `first_native NAME`

  prints the first `NAME` on PATH that `native_program` takes, each `type -ap` line a whole path, spaces and all, and fails naming each candidate it passed.
  A rig whose `omp` or `claude` may be a wrapper finds the program itself with it

- `omp_program PIN`

  prints the program a Legion process this shell starts runs for `omp_invocation: mise x PIN -- omp`: `LEGION_OMP_PATH`'s real path when it is set, since it outranks the invocation, and PIN's own `bin/omp` otherwise

`lib/process-tree.test.ts` (`bun test scripts/e2e/lib`) runs each against processes it starts, under an `ERR` trap that prints:
the pid file names the process the command execs into and nothing once it ends; a pid running another program, and one of the same program with another start time, are refused;
`launched` finds a program below `bash` and `sh -c`, and beside a helper child of its parent, which `child_of` refuses as two children, and refuses two matches, none, and a tree where only another program runs;
and the processes of the rows that start a line or a helper are gone when they end;
and `first_native` takes the first program itself on PATH, in a directory whose name holds a space, past a script and a mise shim.
Its process-scan rows put a `ps`, `pgrep` and `pkill` on `PATH` that log each call and run nothing: `signalable` refuses the calling shell and, asked from a child shell that does not lead its group, that group's leader, `stop_tree` leaves a process whose parent is not the one it is given and then ends a parent and its two children, the cleanup wait's failure names each survivor that runs with its command line and none that has ended, and none of them starts any of the three.
Where `strace` is on the host, a traced row holds that they open no other process's `/proc` entry, list no directory but the asked processes' `task` directories, and read no other `/proc` file than `/proc/stat` and bash's own `/proc/self/maps`; against the helpers before them each row fails.

## lib/nats-image.sh

Sets `nats_image` to the NATS server image every script that starts a NATS server of its own in a container runs: `testnats.HubImage`, the constant `packages/envoy/internal/testnats/nats.go` declares, which is the hub's `nats:2.15.1-alpine` (`.github/ci-images.yaml`).
The listener and the Legion daemon refuse a nats-server below the floor `packages/envoy/natsversion` holds, every 2.10 among them, so a proof on any older image never brings its stack up; reading the Go tests' own constant keeps the two from drifting.
Needs `$root`; ends the script when it cannot read the constant, and says so on stderr under `set -e` too.
The operator walkthrough's `record-casts.sh` and pi-legion's skill-scenario rig source it.
`lib/nats-image.test.ts` scans every tracked shell script, and holds each one with a `docker run` line that passes `-js` to starting `"$nats_image"` and sourcing this helper; the drivers above are a floor under that scan.
It also scans every tracked text file but dated plans and changelogs for the nats-server module's path followed by `@` and a version, in any command (`go install`, `go run`, `go get`), a literal version or `@$name` with a literal `name=<version>` in the same file, and fails on any version the floor refuses.
It holds no copy of the floor: it builds `packages/envoy/natsversion/cmd/natsfloor` once and asks it, so the verdict is `natsversion.Refusal`'s, the rule the listener and the daemon refuse a server by, and so it needs `go` on `PATH`.
It does not see a `docker run` continued over lines, `podman` or a launcher that is not a shell script, an `export name=` or `${name:-<version>}` assignment, or a version set in another file the script sources; none is used in the tree.

## lib/omp-home.sh

The run's own `HOME` for Oh My Pi, so a script's isolated profile lives in its work directory and the operator's `~/.omp/profiles` is never written or deleted.
Stage 2, `controller-start-tmux.sh` and `dispatch-user-turns.sh` source it, and so do the two model-route libraries ([`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh) and [`lib/install-key-file-model-route.sh`](#libinstall-key-file-model-routesh)) for `omp_setup_wizard_off`.

```sh
. "$root/scripts/e2e/lib/omp-home.sh"     # sourced, never run
make_omp_home "$work/omp-home"
omp_setup_wizard_off "$work/omp-home/.omp/profiles/$profile/agent/config.yml"
env "${omp_session_unsets[@]}" HOME="$work/omp-home" OMP_PROFILE="$profile" "$work/legion" controller start --config "$work/controller.yaml"
```

Oh My Pi places a profile under its config root: `os.homedir()/.omp/profiles/<name>` (`getProfileConfigRoot`, `@oh-my-pi/pi-utils` `dirs.ts`), or `profiles/<name>` under the directory `OMP_CONFIG_ROOT` names, which the pinned build reads and which the driver's own shell is taken not to set.
`XDG_DATA_HOME` moves only a named profile's data, and only once `$XDG_DATA_HOME/omp/profiles/<name>` exists, so the profile's `agent/models.yml` and `config.yml` would stay in the operator's home;
`PI_CONFIG_DIR` and `OMP_CONFIG_ROOT` reach no pane, since neither daemon's pane allow-list carries them.
So the driver runs every Oh My Pi process of the run under `HOME=<dir>`:
the daemon, and through the pane allow-list every pane, the controller, and each `omp` the libs run (`--home`).

`make_omp_home DIR` creates `DIR/.omp` and links `DIR/.omp/natives` to the operator's native module cache ([The natives download](#the-natives-download)), so the run downloads nothing and removing `DIR` removes the link, never the cache.
A second call on the same `DIR` replaces the link rather than writing one inside the cache through it (`ln -sfT`), and a real directory at `DIR/.omp/natives` fails the call.
It first exports `MISE_DATA_DIR` and `XDG_CONFIG_HOME` as the operator's own, the paths each already resolves to, so what the daemon itself runs under `DIR` finds what it did under the operator's `HOME`:
`mise` the same pinned Oh My Pi, `secrets` its secretsd config (the daemon resolves `provider_keys` with it), gh and jj their config.
Panes are unaffected, since the daemon moves their four XDG base directories under `<state_dir>/home` (LEGION-206 P1).

jj reads a leading `~/` in a config value against `HOME`, which is `DIR` once the daemon runs.
An operator's `signing.key = "~/.ssh/<key>.pub"` would then name a key inside `DIR`, where there is none.
With `signing.behavior = "own"`, the first commit of every workspace clone the daemon makes is authored by the operator, so it fails to sign, and no pane ever starts:
the daemon log shows each provision failing with `Couldn't load public key <dir>/.ssh/...`, and the run fails `panes-pinned-to-the-rig-before-any-agent-turn` waiting for the architect to register.
So `make_omp_home` pins every `~/` value of the operator's jj config, in `DIR/.jjconfig-operator-paths.toml`, to the path it names under the operator's `HOME`.
It exports `JJ_CONFIG` as the operator's own config files (`jj config path --user`, which names jj's default files when `JJ_CONFIG` is unset), with that overlay last, so the daemon's jj signs as the operator's does.
A second call leaves its own overlay out when it reads the operator's config.
A `~/` value it cannot pin, inside an array or a table or under a quoted key, fails the call, naming the key.
Panes commit as their App's identity (`JJ_USER` and `JJ_EMAIL`), which `"own"` never signs.

A run's panes and controller therefore see none of the operator's `~/.gitconfig`, `~/.claude`, `~/.codex` or `~/.aws`, which is what a Sandbox pod sees.
The gateway key command alone runs under the operator's `HOME` ([`lib/install-model-gateway.sh`](#libinstall-model-gatewaysh)).

`omp_setup_wizard_off FILE` appends `startup.setupWizard: false` to `FILE`, the `agent/config.yml` of a profile the run writes, and every writer of a profile that an interactive Oh My Pi opens calls it: the three model-route libraries and the controller proof.
An interactive Oh My Pi in a terminal, such as the one `legion controller start` runs in a tmux pane, opens its first-run setup wizard (`Setup step 1 of 5`, `Sign in to your providers`) while the profile's `setupVersion` is below the build's `CURRENT_SETUP_VERSION`, and on a fresh profile the wizard blocks the session's first command and normal editor interaction (`runInteractiveMode` in `packages/coding-agent/src/main.ts` and `selectSetupScenes` in `packages/tui/src/setup/wizard.ts`, at the pin).
`startup.setupWizard: false` shuts the wizard whatever setup version the build counts to; an Oh My Pi in `--mode rpc`, as every daemon pane runs, never opens it.
A named profile's own settings file is its `agent/config.yml`, and it inherits no other `config.yml` of the home (`getProfileConfigRoot`, `@oh-my-pi/pi-utils` `dirs.ts`).
Only the working directory's project settings and `PI_CONFIG_FILES` overlays merge over it, so each profile the run writes carries the line itself.

`omp_session_unsets` is the one list of the operator's own Oh My Pi session variables that no Oh My Pi a rig starts from that session may inherit, as `env` options:
`-u OMP_SESSION_ID -u OMPCODE -u PI_CONFIG_FILES -u ENVOY_NATS_URL -u LEGION_OMP_PATH -u OMP_SESSION_EXCLUSIVE`.
They are that session's id, the mark Oh My Pi puts on the shells its bash tool spawns, its settings overlays (laid over every profile Oh My Pi opens), the NATS its Envoy plugin dials, the Oh My Pi binary that `legion controller start` and the daemon would run in place of `omp_invocation`, and the variable the runtime sets to `1` in a pane or pod that resumes a recorded Oh My Pi session, under which an Oh My Pi a rig resumes (`--continue`) exits rather than moving where another live process holds the session, and which `legion controller start` also drops from what it inherits.
`legion controller start` hands Oh My Pi its caller's environment, so the rigs that run it or an interactive `omp` from the operator's shell (`controller-start-tmux.sh`, `dispatch-user-turns.sh`) put the list in front of each launch: `env "${omp_session_unsets[@]}" …` in an argv, and `env ${omp_session_unsets[*]} …` in a pane's shell command.

## lib/leftovers.sh

A driver that dies with no chance to clean up (SIGKILL, a crashed host) leaves its daemon, listener, agents and containers running.
`refuse_leftovers PREFIX` is how the next run of the same script notices:
Stage 2 (`legion-e2e2`), `controller-start-tmux.sh` (`legion-e2e-controller`) and `dispatch-user-turns.sh` (`legion-e2e-user-turns`) run it before they start anything.

```sh
. "$root/scripts/e2e/lib/leftovers.sh"     # sourced, never run
refuse_leftovers legion-e2e2
```

Everything a run starts carries its driver's pid:
each container is named `<prefix>-<role>-<pid>`, and every process of the run holds its work directory, `/tmp/<prefix>.<pid>.<random>`, in its argv, its working directory (an agent) or its environment (a bridge handed the run's state directories).
The check fails, through the caller's `fail`, naming each container and process whose driver is no longer alive and the command that removes it (`docker rm -f -v <name>`, `kill <pid>`).
A driver counts as alive only while its pid is a live, non-zombie process that started no later than the container or process it would own, since pids are reused.
Only processes in the caller's own pid namespace are read, since a pid means nothing across namespaces (an agent box's run is its own).
What a live run started is left alone, since the scripts take no lock and two runs may be going at once.
It reads `${proc_root:-/proc}` (a test points `proc_root` at a proc root of its own, [`lib/proc-root.ts`](#libproc-rootts)), each process's command line and environment in the shell itself and the working directories of the processes neither names a run in through one `find`, so it starts no program for each process.
A proc root that lists no process, and a `find` that failed, that a signal ended or that is too old for `-files0-from`, are reported through `fail`, naming them, with 2; a working directory of another user's process, or of one that ended during the walk, is no complaint ([`proc_walk_complaint`](#libprocess-treesh)).
`lib/leftovers.test.ts` holds each, and the reading of each of the three places a process names its run

## lib/kind-sandbox.sh

A throwaway kind cluster running Agent Sandbox's controller under gVisor, for a test of the Sandbox runtime on an amd64 Linux host.
The kind job of `ci.yaml` drives it from Go: `packages/daemon/internal/kindtest` runs each function in a bash of its own, for the rig's own test and every kind test of the daemon and of the Sandbox runtime.

```sh
. "$root/scripts/e2e/lib/kind-sandbox.sh"     # sourced, never run
```

The caller defines `note` and `fail`, which exits, and calls `kind_sandbox_teardown` when it ends, from its `EXIT` trap or its test's cleanup.
`LEGION_E2E_KIND` names the kind binary when `kind_sandbox_install_kind` has not run in the same shell.

Every pin of the rig is in the file.
kind is v0.33.0, built with `go install`, and `kind_sandbox_create` refuses another version.
The node image, the local registry, busybox, the NFS server and PostgreSQL are named by the digests of their rows of `.github/ci-images.yaml`.
Agent Sandbox is its v1.0.3 manifest, vendored in `packages/daemon/internal/runtime/sandbox/testdata` beside the CRD the runtime's schema tests read; the manifest's header says how it differs from the release.
gVisor is its dated release, pinned by sha512.

Each item below names a function, then what it does.

- `kind_sandbox_install_kind DIR`

  builds the pinned kind into `DIR` and makes it `kind_bin`

- `kind_sandbox_use NAME WORK`

  names the cluster `NAME` and its files in `WORK` for the functions below, and makes no cluster: a caller that runs each function in a shell of its own calls it first in each

- `kind_sandbox_create NAME WORK [API_PORT]`

  creates the cluster `NAME` on the pinned node image, its kubeconfig a file of `WORK` (`kind_kubeconfig`) and its context `kind-NAME` (`kind_context`);
  labels its one node (`kind_node`) `legion.dev/pool=legion`, the pool the runtime places every pod on;
  creates the `legion` namespace and checks kind's `standard` storage class.
  With `API_PORT`, the API server listens on `127.0.0.1:API_PORT`, and its service-account issuer is `https://127.0.0.1:API_PORT`, whose discovery document and keys anyone may read, so a process of the host verifies a pod's service-account token

- `kind_k ARGS…`

  `kubectl` against that kubeconfig and context alone, so the caller's kubeconfig and its current context are never read

- `kind_sandbox_start_registry PORT`

  starts a registry container on the kind network (`kind_registry`), published on the host at `127.0.0.1:PORT`, and has the node pull `localhost:PORT/<image>` from it, so an image pushed there has a registry digest, which `runtime.kubernetes.image` requires

- `kind_sandbox_install_agent_sandbox`

  applies Agent Sandbox's CRD and its vendored manifest, and waits for its controller's rollout

- `kind_sandbox_install_gvisor WORK`

  installs gVisor's dated release, pinned by sha512, into the node's containerd as the `runsc` handler with a `gvisor` RuntimeClass, and fails unless a `runtimeClassName: gvisor` pod's `dmesg` says `Starting gVisor`

- `kind_sandbox_load_image IMAGE`

  pulls `IMAGE`, which must be named by digest, into the node's containerd, so a pod's start never waits on a registry

- `kind_sandbox_route_github ADDRESS PORT`

  sends the cluster's pods to `ADDRESS:PORT` for `github.com` and `api.github.com`: a Service on port 443 with no selector, whose one endpoint is `ADDRESS:PORT`, and a CoreDNS `hosts` rule naming its cluster IP (`kind_github_address`).
  The rule reaches pods alone: the node, and containerd's pulls, resolve through Docker's DNS

- `kind_sandbox_start_nfs_server SEED`

  starts an NFS-Ganesha server in the `nfs` namespace, exporting `/team`, which holds `seed.txt` with `SEED`, and `/scratch`, and sets `kind_nfs_server` to its Service's cluster IP once the node itself mounts `/team` and reads the seed

- `kind_sandbox_start_postgres PASSWORD`

  starts a PostgreSQL server in the `postgres` namespace, whose superuser `postgres` has `PASSWORD`, on a Service the cluster's pods reach at its cluster IP and the host reaches at the node's address on the kind network and the Service's node port, once it accepts connections

- `kind_sandbox_teardown STATUS EVIDENCE`

  when `STATUS` is not 0, writes the cluster's nodes and pods, the `legion` namespace's Sandboxes and volume claims, the descriptions and container logs of the `legion`, `nfs` and `postgres` namespaces' pods and every event to `EVIDENCE/cluster-at-failure.txt`, and the Agent Sandbox controller's log to `EVIDENCE/agent-sandbox-controller.log`; then deletes the cluster and the registry.
  It does nothing before `kind_sandbox_use` or `kind_sandbox_create` named a cluster, and deletes no cluster once it is gone

## lib/h2-settings.ts

Whether a front proxy in front of Dispatch advertises HTTP/2 extended CONNECT (`SETTINGS_ENABLE_CONNECT_PROTOCOL`, RFC 8441).
Dispatch's live updates ride one WebSocket per tab on a connection of its own;
a proxy that advertises it lets a browser open that socket as a stream of the page's shared HTTP/2 connection instead, where the never-ending request dispatch://INFRA-258 fixed can come back.
Run it against a deployment's front proxy:

```sh
bun scripts/e2e/lib/h2-settings.ts https://<dispatch host>/     # exit 0: not advertised
```

It offers `h2` and `http/1.1` over TLS as a browser does and prints one JSON line:
`url`, `alpn` (the protocol the server chose, `false` for none) and `settings` (the server's SETTINGS as Node's HTTP/2 client reads them, `null` without HTTP/2).
It exits 1 when `settings.enableConnectProtocol` is true, naming the origin on stderr;
0 when it is false or the server speaks no HTTP/2; 2 on a usage error, or when it has no answer: a TLS or certificate failure, or no SETTINGS within 10 s.
Certificates are verified, so a proxy behind a private CA needs `NODE_EXTRA_CA_CERTS`.
`h2-settings.test.ts` (`bun test scripts/e2e/lib`) runs it against a TLS server that writes its own SETTINGS frame advertising extended CONNECT (the pinned Bun's `http2.createSecureServer` sends none), against a local `http2.createSecureServer` that does not advertise it, against an https server with no HTTP/2, against a certificate it cannot verify, and against two servers that answer nothing
— one that chooses h2 and sends no SETTINGS, one that never finishes the handshake — which it leaves after the 10 s deadline with exit 2.

## lib/dispatch-live-events.ts

A Dispatch deployment's event feed as a page reads it, for an audit of what a run wrote and for the production check after a deploy.
It opens `/ws/live`, subscribes the event feed and writes one JSON line per frame until an event matches `--until` (exit 0) or `--timeout` seconds pass (exit 1);
Dispatch's refusal of the feed is printed on stderr (exit 1), a usage error exits 2:

```sh
bun scripts/e2e/lib/dispatch-live-events.ts --url https://<dispatch host> \
  --bearer-file <token file> --since <event id> --until '"state":"answered"' --timeout 70
```

`--user <email>` takes the place of `--bearer-file` against a server in header identity (the e2e harness), and `--cookie-file <file>` against one in cookie identity:
the file holds the `Cookie` request header a browser signed in to the deployment sends it, copied from the developer tools' Network panel.
`--from-whoami` starts the feed where a page starts its first one (INFRA-292):
it reads `GET /auth/whoami` as that person, prints `{"whoami": {"event_head": <id>, "login": …}}`, waits `--subscribe-after <s>` seconds (`{"subscribing_in": <s>}`) and subscribes from that `event_head`, so a write committed in the wait - after whoami answered, before the socket opened - is replayed to it;
whoami names a person, so it takes `--cookie-file` or `--user` and no `--since`.
The lines are `{"event": …}` for each event, `{"close": <code>, "reason": …}` and `{"resubscribed": <cursor>}` around each reopen - a socket closed at its age (`4001`) or a restart (`1012`) reopens at once, anything else after a backoff - `{"ended": <reason>, "resubscribed":
<cursor>}` where Dispatch drops the feed and leaves the socket open, and `{"duplicate": <id>}` for an event at or below the cursor, which is written once as an event and never again.
The cursor is Dispatch's own `events_start.since` at every subscribe, since Dispatch starts a feed at what it was asked or below it, never above:
a `--since` past its head, and a resume across a reset of its event log, both start lower, and the events after that start are new rather than duplicates.
So an audit that counts the `event` lines counts each event once, however often the feed reopened.
`dispatch-live-events.test.ts` (`bun test scripts/e2e/lib`) runs it against a Bun websocket server that rotates, ends the feed, repeats events, clamps a start and answers whoami a head under a cookie.

## lib/dispatch-keyed-comment.ts

The production check of Dispatch's keyed writes: a comment whose first answer is lost lands once.

```sh
bun scripts/e2e/lib/dispatch-keyed-comment.ts --url https://<dispatch host> \
  --bearer-file <token file> --issue <scratch issue key> [--cli]
```

It reads `/healthz` and fails unless the body says `idempotency_keys: true` (a 503's body included:
the field is a fact of the build), then posts one comment on the issue it names as the bearer's session (`--session <id>`, `deploy-check-<uuid>` by default).
Without `--cli` it posts the comment twice under one `Idempotency-Key`, as a page whose first answer was lost asks again, and requires the repeat to be Dispatch's stored `201` with `Idempotency-Replayed: ?1`, the same comment id and the key echoed on both.
With `--cli` this checkout's `dispatch comment` (`packages/envoy-client/bin/dispatch.ts`, under `DISPATCH_HOST=omp` and a scratch `HOME`) posts it through a proxy on `127.0.0.1` that forwards everything and hands the command a gateway's `502` page in place of Dispatch's answer to its first comment POST;
the command must ask again under that POST's key, be answered the replay and exit 0.
Either way the issue must then hold exactly one comment with the check's body.
Each step prints one JSON line (`healthz`, `post` or `proxy` and `cli`, `comments_with_body`), then `{"ok":true}` and exit 0;
a failed check prints `{"failed": "<why>"}` and exits 1, a usage error exits 2.
The comment stays on the issue, so name a scratch one.
`dispatch-keyed-comment.test.ts` (`bun test scripts/e2e/lib`) runs it against a stand-in Dispatch that keys comments, one that makes every POST, a `/healthz` without the field and a `503` with it, and with `--cli`.
