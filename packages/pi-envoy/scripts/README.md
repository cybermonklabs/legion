# Pi Envoy diagnostic scripts

## smoke-delivery.sh

End-to-end delivery check of the packed plugin, which the `plugin-smokes` part of `ci.yaml`'s
`agent-harness` job runs as a step (`.github/workflows/ci-job-agent-harness.yaml`). It starts one
real Oh My Pi session in `--mode rpc`, as a Legion pane runs it, whose only extension is this
checkout's pi-envoy packed as the release packs it and whose only tool is bash. The session's
model, a stand-in, calls `envoy_role_set`; the script publishes to that role's namespaced topic
through the listener's HTTP API and requires the session to write the exact payload to
`received.txt`; then it ends the session by closing its input and requires the role to stop
resolving (404) within 15 seconds.

It prints one line per check, `CHECK <name>: PASS` or `CHECK <name>: FAIL: <why>`, and exits 1
after a FAIL: `setup`, `role-claimed` (within 60 s), `delivered` (within 90 s) and `teardown`.
The teardown ends the session the way its host ends an RPC session, which runs the plugin's
`session_shutdown` and its deregistration; Oh My Pi ends a session it is sent a signal by with no
shutdown handler at all, which leaves the role standing until the listener's live window.

### Usage

```bash
LEGION_TEST_OMP=<omp> bash packages/pi-envoy/scripts/smoke-delivery.sh
```

It takes nothing from outside the machine, through `scripts/e2e/lib/plugin-smoke.sh`, the rig the
four plugin smokes share (`scripts/e2e/README.md` describes it):

- `LEGION_TEST_OMP` names the Oh My Pi binary, the build `.omp-pin` pins, which
  `.github/actions/install-omp` installs in CI.
- natsd and the Envoy listener are the ones `packages/envoy-client/test/envoy-stack.ts` builds
  from this checkout with `go build` and runs on loopback ports of their own.
- The model is `scripts/e2e/lib/plugin-smoke-model.ts`, a stand-in on loopback keyed by a
  placeholder of the run's own.
- `PLUGIN_SMOKE_EVIDENCE_DIR`, when set, is where the run's logs go; the CI step sets it.

It needs `go`, `bun` with the checkout's dependencies installed, `curl`, `jq` and `tar`, and no
tmux, Docker, model key or deployment.

## smoke-btw.sh

End-to-end check of a targeted Dispatch message to one live session running the packed plugin,
which the same `plugin-smokes` part runs as a step. For each mode, `btw` and then `steer`, it
starts one Oh My Pi session as `smoke-delivery.sh` does, whose prompt asks for one bash command
that creates a sentinel file and then sleeps 45 seconds; it waits for that session's exact work
directory to appear in the listener's registry and for the sentinel to exist, then targets only
that session with a message on an issue of its own Dispatch while the sleep is still running.
A BTW is answered on the session's side turn and posted by the plugin; a steer is read at the
turn's next tool boundary, and the session runs the `dispatch message` its card's `reply_with`
names, which the plugin puts first on the session's `PATH` from its own `bin/`. Either way a reply
naming the message must appear on the issue within 90 seconds, and the session's end must take it
out of the registry within 15.

### Usage

```bash
LEGION_TEST_OMP=<omp> LEGION_TEST_PG_DSN=<loopback PostgreSQL with pgvector> \
  bash packages/pi-envoy/scripts/smoke-btw.sh [btw|steer]
```

With no argument it runs both modes, one session each. Dispatch is the server
`packages/envoy-client/test/dispatch-stack.ts` builds from this checkout, with its NATS on the
run's natsd and its deliveries through the run's listener, on a database of its own that it makes
on `LEGION_TEST_PG_DSN` and drops at the end. A person of the run's own makes the issue, through
the server's trusted identity header, and the targeted message is an agent's: it is sent with the
server's bearer and names itself in `actor` (`smoke-btw-<hex>`), so the card is agent-authored.
Everything else is as for `smoke-delivery.sh`.

Each mode prints its own checks: `<mode>-registered`, `<mode>-sleeping`, `<mode>-targeted`,
`<mode>-replied` and `<mode>-teardown`, after `setup`. Every wait is a wall-clock deadline, not a
count of polls, so a blackholed service fails at its stated timeout instead of stretching it.

On every exit the scripts stop what they started by the pid each was started as, drop Dispatch's
database and remove their `mktemp -d` under `$TMPDIR`, which a failed run without
`PLUGIN_SMOKE_EVIDENCE_DIR` keeps and names.

## smoke-role-takeover.sh

Live proof that a role claim is weighed on the process that holds it: a second process that
resumed a live session's id can neither take, release nor outlast that session's role
(dispatch://INFRA-783), and a soft claim naming a predecessor takes the role only from a
predecessor that is gone or is the claimant's own client instance (dispatch://INFRA-404, R8 and
A5 of its spec); and once a session that holds a role restarts, the role lookup and the session
listing name the same holder (dispatch://INFRA-2747). It needs no model at all, not even a
stand-in, and no deployment: `go` and `bun` alone, and CI runs it as a step of the
`envoy-session-stall` job.

It builds natsd (`packages/envoy/internal/testnats/cmd/natsd`) and the listener
(`packages/envoy/cmd/listener`) from the checkout and runs both on loopback ports of its own, as
[`packages/envoy-client/test/envoy-stack.ts`](../../envoy-client/test/envoy-stack.ts) does, and
drives them with real client processes:

- one Bun process per Oh My Pi Envoy extension instance (`role-takeover/omp-client.ts`), each
  loading this checkout's `extensions/envoy.ts` against the stand-in host the package's own suite
  uses — one session manager whose id and branch the driver moves, and a heartbeat the driver
  ticks rather than a timer. One process is one client instance, with one client instance id;
- the real Claude Code channel server (`packages/claude-envoy/bin/envoy-channel.ts`), spawned and
  driven over MCP by `role-takeover/claude-client.ts`, which is also the parent process whose
  handoff file a `/clear` writes, so the server adopts the new session id on its own.

Every client reaches the listener through a recording proxy of its own
(`role-takeover/listener-proxy.ts`), so each request and the listener's answer to it is read from
a log rather than inferred.

[`scripts/e2e/nats-callout-sessions.sh`](../../../scripts/e2e/README.md#nats-callout-sessionssh)
drives the same two clients as the sessions of its stack behind the NATS auth service, with no
proxy, since a broker token's proof names the listener's own origin: `omp-client.ts` answers
`{"op":"deliveries"}` with what the extension handed the host, and `claude-client.ts` takes
`--env-file` (the variables a broker trade reads, added to the server's environment) and
`--inbox` (it plays the Legion inbox the server writes each delivery to, which
`{"op":"deliveries"}` then answers). Beside them it drives a third, `role-takeover/codex-client.ts`,
which the script compiles into a binary named `codex`: the `codex` process of one Codex CLI session,
which starts the installed Codex plugin's MCP server with only its manifest's `env_vars` and the
variables Codex hands every MCP server, runs the plugin's hooks (`{"op":"start"}` and
`{"op":"new"}` run `SessionStart`), keeps the transcript the plugin reads, and answers the
plugin's `codex queue`, whose messages `{"op":"deliveries"}` then answers. `role-takeover/codex-probe.ts`
runs `natsprobe` for the script's Codex probe rows in the environment a Codex plugin server got
(its `/proc/<pid>/environ`), presenting the credential the plugin's own settings resolve there.

The listener runs with windows of its own, because every row turns on whether a claim's holder is
still answering for it: `ENVOY_SESSION_LIVE_WINDOW=10s`, which is how long a claim stands renewed
after its holder's last registration, and `ENVOY_ROLE_CLAIM_REAP_INTERVAL=2s`, so every row is
swept several times. The one claim the reaper frees in the run is the one a hand-back marked:
no registration renews a marked claim, so it goes within one more reap interval of its renewal
lapsing, though the client that handed it back goes on registering that session id.
`ENVOY_SESSION_GONE_HORIZON` keeps its hour-long default all the same, since every unmarked
claim is kept while its holder's session reads short of `gone`, which is what leaves every
other row's holder in its role. The rows still read the listener's own word for each outcome — a claim
granted, a `409`, or the lookup's `404` — which reads the same whether a standing claim lapsed
where it stood or a pass had already removed it, and a row whose claimant must be refused is
decided inside the window, its holder having answered for the claim immediately before. A row
that waits the window out waits for the listener to say so, never for a sleep.

Eighteen rows are printed, each with its outcome:

- **row 1**, twice — a claim under the id that holds the role is granted because nobody renewed
  the standing claim inside the live window: an Oh My Pi plugin reload, and a Claude Code channel
  server restart, each under the session id that holds the role.

- **row 2** — an Oh My Pi fork gets its parent's role, because the parent's live entry carries the
  claimant's own client instance id.

- **row 3** — an Oh My Pi move, in one process, to a session whose records name the held role gets
  it, by the same case.

- **row 4** — a Claude Code `/clear` handoff gets the role: the deregistration removes the
  process's own entry, and the claim then finds no live predecessor.

- **INFRA-783**, four rows — a second process that resumed a live session's id is refused the role
  that session holds (`409` naming it, in Oh My Pi and in Claude Code); the Oh My Pi one is then
  granted the role once the holder stops renewing the claim and the window passes; and the
  process that no longer holds the claim hands the role topic back, which frees nothing.

- **row 5**, three rows — a claim naming a predecessor that is live inside another client
  instance is refused `409` naming it, in Oh My Pi (A5 scenario 1), in a move made by that same
  refused process, and in the Claude Code `/clear` handoff (A5 scenario 2). The live predecessor
  is built from two session ids, since a second process claiming under one id is now decided on
  the standing claim rather than on the predecessor it names.

- **A5**, two rows — a refused process stays registered, and after the holder exits the refused
  Oh My Pi process takes the role on its next session event.

- **INFRA-783**, three further rows — an older client's hand-back, which is a release of a
  role's own lane topic under the holder's session id stating no `client_instance_id`, frees
  nothing and marks the claim: the lookup answers `handed_back`, the real Oh My Pi holder reads
  that as a role it does not hold, claims it back at its next heartbeat and takes the lane's
  next message. The same hand-back against an old-build holder, driven by `curl` because a
  current client would take the role straight back, frees the role a live window and up to one
  reap interval later while
  that client is still registering. And a client that states no process at all — the shape every
  claim standing at a deploy is in — keeps its role across three live windows of registrations
  that carry no `client_instance_id`.

- **INFRA-2747**, one row — a session that holds a role restarts: its Oh My Pi process is
  killed, a new one starts under the same session id and is granted the role once the live
  window passes, and within 90 seconds of that process's first registration
  `GET /v1/roles/<role>` and `GET /v1/sessions?include=unresponsive` name the same holder, or
  both name none. The lookup reads the claim record and the listing the listener's copy of the
  role claims, so a copy that lost the claim shows as the holder listed with no role. The
  lookup's holder is read off its `200` and off its `404` (`holder_lapsed`) alike, since a
  holder nothing renews is lapsed ten seconds on and still holds the role, and a reading counts
  only where the lookup answered one of those and the listing `200`. The row prints how long
  after the first registration the two agreed.

### Usage

```bash
bash packages/pi-envoy/scripts/smoke-role-takeover.sh
bash packages/pi-envoy/scripts/smoke-role-takeover.sh --base-rev main@origin   # every row passes at b601d25b
bash packages/pi-envoy/scripts/smoke-role-takeover.sh --listener-bin <built listener>
SMOKE_DROP_ROLE_FROM_COPY=1 bash packages/pi-envoy/scripts/smoke-role-takeover.sh \
  --listener-bin <scratch build that serves the drop route>   # the INFRA-2747 row's control
```

`--base-rev <rev>` builds only the listener from that revision, leaving every client as it is in
the working copy. Against `main@origin` at `b601d25b` (2026-11-13) none of the eighteen rows
fail. The eleven of seventeen that once failed there were counted in cybermonklabs/legion#2875
(`277768d83e`, 2026-11-07) against the main of that day, which did not yet have the listener rule
those rows drive; main has carried that change since, so `--base-rev main@origin` is no longer a
negative control for them. The INFRA-2747 row passes there too: with nothing taken out of its
copy, that listener's copy names the restarted holder's role as the claim record does, and that
row's negative control is `SMOKE_DROP_ROLE_FROM_COPY=1` against a scratch build, below. It reads
the revision without touching the working copy (`jj --ignore-working-copy log` resolves it,
`git archive` extracts `packages/envoy`), moves no bookmark and writes nothing outside its
scratch.

`--listener-bin <path>` runs that built listener in place of building one, so a control can run a
scratch build; natsd is still built from the checkout. A path that is not an executable file is
refused with exit 2, and so is `--listener-bin` beside `--base-rev`, since each chooses the
listener.

`SMOKE_DROP_ROLE_FROM_COPY=1` turns the INFRA-2747 row into its own control
(c-live-restart of dispatch://INFRA-2747). Once the restarted process's claim is granted, the row
asks the listener to take the role out of its copy of the role claims
(`POST /debug/role-copy/drop?role=<role>`) and fails, saying so, on any answer that is not a
`2xx`, so a listener without that route never passes the control by doing nothing. It then reads
the listing once and requires that the drop took the role out of it, sends the process no
heartbeat, and requires at the end that the process sent no registration and no claim, so
nothing but the listener's own repair can put the role back in the copy. The comparison starts
once the drop has answered and keeps the same window. Only a scratch build serves the route: a
patch that adds it is applied to a `git archive` of the revision under `$TMPDIR`, built there with
`go build ./cmd/listener`, and passed with `--listener-bin`. The patch is never committed. A
listener that repairs its copy within the window passes the row; one that does not lists the
holder with no role until the window ends, and the row fails naming the two answers it last read.

Prerequisites, each checked before anything starts: `bash` 5 or later, `go`, `bun` with the
checkout's dependencies installed, `curl`, `jq`, and `jj`, `git` and `tar` for `--base-rev`.
Everything the run writes — the binaries it builds, the JetStream store, each client's home and
state directory, every proxy's request log — lives in one `mktemp -d` under `$TMPDIR`, which an
exit trap removes after a pass and keeps after anything else. Nothing it starts inherits this
shell's environment (`env -i` plus `PATH` and what the script sets), so an operator's `ENVOY_*`,
`NATS_*` or `DISPATCH_*` variables never reach a client. A local run takes about two minutes, and
about four with `--base-rev`.

### Exit codes

| Code | Meaning |
|------|---------|
| 0 | `SMOKE ROLE TAKEOVER PASS:` every row held. |
| 1 | `SMOKE ROLE TAKEOVER FAIL:` the rows that did not hold are listed, each with the check that failed and what it saw. |
| 2 | Refused: a prerequisite is missing, the configuration is invalid, or a client or service did not start. |
| 129, 130, 143 | Interrupted by SIGHUP, SIGINT or SIGTERM; cleanup still ran. |

## smoke-registration-stall.sh

Live check of the registration-stall fix, which the same `plugin-smokes` part runs as a step: a
real headless `omp` pane running the packed plugin, against a real Envoy listener, watched through
the live registry. `--arm stall` shows that a pane whose registration post is wedged forever keeps
refreshing past one live window and says so exactly once; `--arm recover` shows that a pane
unregistered while its process runs re-registers itself. `--control` is each arm's negative
control. The stall control SIGKILLs its pane at the wedge, as a pane whose refresh never comes
back is, and requires its record to be listed after the kill, leave the registry within the live
window and a margin, and never return: the window the stall arm's pane outlives is one this
listener enforces. The recover control SIGKILLs its pane before the delete and requires the record
to stay gone, so the recover arm's return is the live pane's own refresh. That a wedged pass no
longer holds every later tick is what the plugin's unit tests hold (`extensions/envoy.test.ts`, "a
pass stuck on a listener call that never answers still lets later ticks refresh the registration"
and "a whole stall says so once in the pane and once in the log").

```bash
LEGION_TEST_OMP=<omp> bash packages/pi-envoy/scripts/smoke-registration-stall.sh
LEGION_TEST_OMP=<omp> bash packages/pi-envoy/scripts/smoke-registration-stall.sh --arm stall --control
```

With no `--arm` it runs both arms and both controls, one pane each, which is what CI runs. Each
prints `CHECK <arm>: PASS` or `CHECK <arm>: FAIL: <why>` after `setup` (`stall`, `stall-control`,
`recover` and `recover-control`), and the run exits 1 after a FAIL and 2 on a refused argument.

The listener is the one `packages/envoy-client/test/envoy-stack.ts` builds, as for
`smoke-delivery.sh`, given a live window of its own (`ENVOY_SESSION_LIVE_WINDOW`), and the pane's
one prompt is answered by the same model stand-in. The pane refreshes every 20 seconds, or every
`--heartbeat-ms <n>`, and its pass deadline is that interval, as a deployment's is. The window is
kept in the arrangement `@legion/contracts` declares, which the script reads at run time
(`SESSION_HEARTBEAT_MS` and `SESSION_LIVE_WINDOW_MS`): three refreshes and one whole listener
call fall short of it with the deployment's slack to spare, so it is
`SESSION_LIVE_WINDOW_MS - 3 × (SESSION_HEARTBEAT_MS - refresh)`, 90 seconds at the default. Every
budget the run prints is measured in those two numbers.

## local-stack.sh

The loopback Envoy stack `smoke-role-takeover.sh` brings up, in one sourced file: a free loopback
port below the kernel's ephemeral range (`port_range`, `port_open`, `pick_port`), the natsd and
listener builds (`build_natsd`, `build_listener`, each taking the `packages/envoy` directory to
build from, which is how `smoke-role-takeover.sh --base-rev` builds the listener from another
revision), the argv and assignments each service is run with (`set_natsd_argv`, `set_listener_env`)
and when it is up (`natsd_serving`, `listener_healthy`, polled with `poll_until`). It is sourced,
never run: it starts no process, so the script keeps its own isolation, logs, traps, timeouts,
messages and exit codes. A caller defines `refuse` and sets `local_stack_fail` to the function an
exhausted port range is reported through, `refuse` in that smoke.
`packages/envoy-client/test/envoy-stack.ts` is the same stack for the TypeScript suite, and the
one the other three smokes here run.

## listener-api.sh

The listener's own HTTP surface, in the second sourced file `smoke-role-takeover.sh` brings in
beside `local-stack.sh`: one raw call either way (`listener_get`, `listener_post`), the three
requests a client built before dispatch://INFRA-783 sends, which that smoke drives by `curl`
because no client in the checkout can be made to send them (`old_register`, `old_claim`,
`old_handback`), and the readers every row states its outcome in — one field of
`GET /v1/roles/<role>` (`role_field`, and `role_holder`, `role_instance`, `role_handed_back`,
`role_reason`, `role_unrenewed` over it) and the registry's answer for a session (`registered`,
`unregistered`). It is sourced, never run, and reads three variables the caller sets:
`envoy_url`, `work` and `curl_max_time`.

## role-takeover/clients.sh

The clients themselves, in the third sourced file `smoke-role-takeover.sh` brings in, beside the
Bun drivers it starts: a recording proxy and a process per client (`start_proxy`, `start_omp`,
`start_claude`), the stdin and stdout one command is sent and answered over (`client_fifo`,
`client_ready`, `send`), the readers of each client's proxy log — the claims it made
(`claim_status`, `claim_holder`, `first_claim_status`, `first_claim_holder`, `made_claim`,
`claim_count`), the registrations it sent (`instance_id`, `registered_at`,
`first_registered_at`), the requests of either kind (`claim_writes`), its deregistration
(`delete_answer`) and its hand-backs (`release_status`, `release_instance`, `released`) — and the
two ways a row makes a holder answer for its claim (`renew_omp`, `renew_claude`). It is sourced,
never run: it starts no process of its own, and it reads the scratch directory, the driver
directory, the checkout, the listener and natsd URLs, the Oh My Pi client's windows, the two
client timeouts and the run id that the smoke sets.

## skill-scenarios/rig.sh

Replays skill scenarios on real agents, to compare two versions of a skill. Each label is one
checkout's packed plugin in an isolated Oh My Pi home (`rig.sh profile <label> <checkout>`); each
run is one fresh `omp -p` agent given one task a skill rule governs, scored afterwards from what the
stand-ins and the scratch Dispatch recorded (`rig.sh score`). `rig.sh live-read <label> <skill>...`
has one session read every file of a skill and reports whether each arrived whole.

```bash
cd packages/pi-legion/scripts/skill-scenarios
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic ./rig.sh profile head <checkout>
# or, with no gateway: LEGION_E2E_MODEL_ROUTE=key-file LEGION_E2E_ANTHROPIC_KEY_FILE=<0600 file>
# or, through a provider block of your own models.yml whose apiKey is `!cat <absolute file>`:
#   LEGION_E2E_MODEL_ROUTE=provider LEGION_E2E_MODEL_PROVIDER_FILE=<models.yml> LEGION_E2E_MODEL_PROVIDER=<block>
./rig.sh batch ask-on-message 5 head base   # starts and stops Postgres, NATS, the listener, and each run's own scratch Dispatch
./rig.sh batch tester-proof 5 head base
./rig.sh score
```

The spec-writing and spec-rewrite scenarios measure how an agent writes a Dispatch spec:

```bash
bun fetch-spec-sources.ts --out "$SKILL_SCENARIOS_WORK/sources" <issue key | dispatch://PROJECT/artifact/slug>
SKILL_SCENARIOS_SUPERPOWERS=<superpowers plugin> ./rig.sh profile main <checkout>
SKILL_SCENARIOS_SOURCE=<directory under sources/> ./rig.sh batch spec-writing 1 main
./rig.sh judge "$SKILL_SCENARIOS_WORK"/runs/spec-writing-main-1
./rig.sh score spec-writing
```

The scenarios and every input are in the script's header (`./rig.sh` with no arguments prints it),
and each scenario's scoring rule is on the `score.ts` function that scores it. It needs a model
route (the gateway, an Anthropic key file, or a provider block of the operator's own models.yml)
and Docker; every container a batch or a judge starts has CPU and memory limits.
`SKILL_SCENARIOS_WORK` must have no `.omp` in it or above it, so not under your home directory: Oh
My Pi would load that directory's plugins and not the label's. Its bounded waits, port picks and
process stops are
[`scripts/e2e/lib/rig.sh`](../../../scripts/e2e/README.md#librigsh)'s. Nothing a batch runs reaches
production Dispatch, GitHub, Envoy or NATS: it starts nothing with the caller's `DISPATCH_*`,
`ENVOY_*` or `NATS_*` variables, and `seed.ts` refuses to run while `PLAYWRIGHT_BASE_URL` or
`E2E_AGENT_TOKEN` is set. That is the environment only: an agent reads the whole filesystem, the
other label's checkout included, so `score.ts` does not score a run that read outside its own
label. A judge is held tighter: `rig.sh judge-omp` runs each one in a Docker container that sees
only its own directory, read-only but for its one answer file and its transcript, so a judge
cannot read the run's transcript or another judge's input. On the key-file route the container
reads the key file itself. On the gateway route the gateway's key command runs on the host, once
for each judge, because it needs the operator's home and session bus. The container gets only the
token, from an owner-only file that is removed when the judge ends. On the provider route the
profile's models.yml is the block as the operator wrote it, `!cat` command included, so no key is
copied anywhere: the container mounts the directory of the file that command reads, read-only.

Two tools read production Dispatch, with GET requests only, as the calling session does (its
envoy.json, or `DISPATCH_URL` with a token): `fetch-spec-sources.ts`, which writes a source's input
into the work directory for the spec scenarios, and `label-comments.ts`, which labels the comments
people wrote in a window and prints the share that complain about writing. Neither writes inside a
checkout. Text either one reads from production stays in the work directory and is never committed.
