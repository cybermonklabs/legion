# Pi Envoy diagnostic scripts

## smoke-delivery.sh

Real end-to-end delivery smoke test against the *installed* pi-envoy
plugin. It launches a real, interactive `omp` session (TUI mode, never `-p`)
in a scratch tmux session and a throwaway directory outside any repo
checkout, so the only extension that loads is whatever is materialized at
`~/.omp/plugins/node_modules` — never a local source checkout via a repo's
own `omp.extensions` manifest. The session runs on the smoke's own tmux
server, on a socket in that throwaway directory, and the script prints the
`tmux -S <socket> attach -t <session>` line that watches it. It drives the
session with `tmux send-keys` to
call `envoy_role_set` and wait for a message, publishes to that role's
namespaced topic through the live Envoy HTTP API, asserts the session wrote
the exact payload to disk, then kills the session and asserts teardown: the
tmux session disappears and the role stops resolving (404) within 15 seconds.

### Usage

```bash
cd packages/pi-envoy/scripts && ./smoke-delivery.sh
```

Env overrides:

- `ENVOY_URL` — Envoy listener base URL (default `http://127.0.0.1:9020`).
- `OMP_BIN` — the `omp` binary to run (default `omp`, resolved on `PATH` —
  the dotfiles wrapper that injects provider auth).

Run from a host with:

- A live Envoy listener reachable at `ENVOY_URL`, with NATS behind it.
- The `pi-envoy` plugin installed for every `omp` session on the
  machine, materialized at
  `~/.omp/plugins/node_modules/@sjawhar/pi-envoy`. The script fails
  fast and names the path if that package.json is missing, is not valid
  JSON, or has no `.version` — the smoke is meaningless without the real
  installed artifact.
- A configured model provider: the script makes a real model call inside the
  spawned session, so it costs tokens and needs live credentials.

CI does not run this script — `.github/workflows/ci.yaml` has no
step for it, and none should be added while these prerequisites are unmet in
CI's sandbox.

## smoke-btw.sh

Real targeted Dispatch smoke against the installed plugin. It starts one isolated OMP TUI session
whose initial message (the prompt is `omp`'s argument, not typed keystrokes) asks for one bash
command that creates a sentinel file and then sleeps 45 seconds, waits for that session's exact
work directory to appear in the live listener registry and for the sentinel to exist, then targets
only that session with a message on an existing open Dispatch issue while the sleep is still
running. A correlated reply must appear on the same issue before the script tears down the session
and confirms its registry entry clears. It never targets a role or an existing session. The
spawned session is pointed at the same Dispatch the script talks to (`DISPATCH_URL`, plus a 0600
token file when the script holds a bearer), so its reply lands where the script looks for it.

### Usage

```bash
cd packages/pi-envoy/scripts && MODE=btw DISPATCH_SMOKE_ISSUE=CORE-1 ./smoke-btw.sh
```

Set `MODE=btw` (the default) for an ephemeral reply, or `MODE=steer` to prove the primary-turn
delivery path. `ENVOY_URL` defaults to `http://127.0.0.1:9020` and `OMP_BIN` defaults to `omp`.
`DISPATCH_SMOKE_ISSUE` names the open issue that will receive the smoke card.

Who asks: by default the script is an agent. It asks the Dispatch a command run from this shell
resolves, with that command's bearer
([`scripts/e2e/lib/operator-dispatch.sh`](../../../scripts/e2e/README.md#liboperator-dispatchsh),
which runs `bun` and needs the checkout's dependencies installed): `DISPATCH_URL`,
`DISPATCH_TOKEN_FILE` and `DISPATCH_TOKEN` over the `dispatch` section of the envoy.json
`envoyConfigPath("command")` chooses, whose token goes only to the server written beside it and
whose `!` token runs. It names itself in the message's `actor` as `SMOKE_ACTOR_SESSION` (default
`smoke-btw-<hex>`; pass your own Envoy session id when you run it from a session), so the card on
the issue is visibly agent-authored. With no Dispatch configured it refuses, saying why; any other
refusal of the client's (a file's token that `DISPATCH_URL` names another server for, a token file
that cannot be read) stops it naming the reason. Set `DISPATCH_AUTH_HEADER` to one
human-authenticated HTTP header (the current Dispatch cookie or a trusted identity header) to ask as
a human instead, exactly as the dashboard would: `actor` is then omitted, and the script asks
`DISPATCH_URL`, default `http://127.0.0.1:8766`.

The script requires live Envoy, Dispatch, and model credentials; it is never a fake-listener test or
a CI step. A 404 from the targeted message path prints `SKIP` and exits zero, meaning the server does
not support targeted messages. A `400 ACTOR_KIND` on a bearer run prints `SKIP` too, meaning
the server requires a human-authenticated targeted message; use a compatible server or pass
`DISPATCH_AUTH_HEADER`.

### Timeouts

Session registration and post-kill teardown bound infrastructure round trips only.
Delivery (90s) is generous by comparison because that window spans real Envoy delivery
plus the time for a live model turn to notice the targeted message and post the correlated
Dispatch reply. Every wait is a wall-clock deadline (not a fixed iteration count), so a
blackholed Envoy fails at its stated timeout instead of quietly stretching it.

### Exit codes

| Code | Meaning |
|------|---------|
| 0 | `PASS:` delivery, correlated reply, and teardown verified; or `SKIP:` server lacks this smoke path. |
| 1 | Any assertion failed — see the `FAIL:` line for which one, plus a captured pane or HTTP status. |

Cleanup (`tmux kill-session`, `rm -rf` the temp dir) runs in an exit trap on
every exit path, so a failed run leaves nothing behind either. Both smokes run
their tmux on a private socket in their temp dir, so the kill can end only the
session the smoke started; on the shared default server it could end anyone's.
Every tmux call in either script goes through the script's `tmux` function,
which names that socket, and a new call goes through it too.

## smoke-role-takeover.sh

Live proof that a role claim is weighed on the process that holds it: a second process that
resumed a live session's id can neither take, release nor outlast that session's role
(dispatch://INFRA-783), and a soft claim naming a predecessor takes the role only from a
predecessor that is gone or is the claimant's own client instance (dispatch://INFRA-404, R8 and
A5 of its spec); and once a session that holds a role restarts, the role lookup and the session
listing name the same holder (dispatch://INFRA-2747). It is the only smoke here that needs
neither a model provider nor a deployment: `go` and `bun` alone, so CI runs it as a step of the
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
a log rather than inferred. Each proxy, like natsd and the listener, binds a port the kernel
chooses and reports it, and the smoke reads the port from that report.

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

Manual live smoke for the registration-stall fix (dispatch://INFRA-246): a real headless `omp`
pane against a real Envoy listener, watched through the live registry. `--arm stall` shows that a
pane whose registration post is wedged forever keeps refreshing past one record lifetime and says
so exactly once; `--arm recover` shows that a pane unregistered while its process runs
re-registers itself. `--control` is each arm's negative control (the stall control needs
`--base-rev <rev>`, the plugin as it was then).

```bash
# against a deployment
ENVOY_URL=<listener> ENVOY_TOKEN_FILE=<0600 file> \
  bash packages/pi-envoy/scripts/smoke-registration-stall.sh --arm recover
# against a listener this run starts itself
bash packages/pi-envoy/scripts/smoke-registration-stall.sh --arm recover --local-stack
```

`--local-stack` builds natsd and the listener from the checkout and runs both on loopback ports of
its own, as `smoke-role-takeover.sh` does, so the run needs no deployment and no bearer: it ignores
`ENVOY_URL` and `ENVOY_TOKEN_FILE`, says so, and keeps both out of the pane it spawns. Its one
remaining live dependency is the model provider the pane's turn needs, which the pane inherits
from the caller's environment. `--heartbeat-ms <n>` shortens the pane's refresh interval, which
every budget the run prints is measured in. Its scratch is a `mktemp -d` under `$TMPDIR` that an
exit trap removes. Exit 0 is `SMOKE <arm> PASS:`, 1 is `SMOKE <arm> FAIL:` naming the observation
that failed, and 2 is a refusal.

## local-stack.sh

The loopback Envoy stack the two smokes above bring up, in one sourced file: the natsd and
listener builds (`build_natsd`, `build_listener`, each taking the `packages/envoy` directory to
build from, which is how `smoke-role-takeover.sh --base-rev` builds the listener from another
revision), the argv and assignments each service is run with (`set_natsd_argv`, `set_listener_env`),
when it is up (`natsd_serving`, `listener_healthy`, polled with `poll_until`) and the ports it
reports it bound (`natsd_port`, `listener_addr`). Each service binds port 0 and names the port the
kernel gave it: no port is picked free and handed to a service, since another process can take
such a port before the service binds it. It is sourced, never run: it starts no process, so every
script keeps its own isolation, logs, traps, timeouts, messages and exit codes.
`packages/envoy-client/test/envoy-stack.ts` is the same stack for the TypeScript suite.

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
has one session read every file of a skill and reports whether each arrived whole; it writes
`$SKILL_SCENARIOS_WORK/live-read/<label>` and refuses a label whose earlier live read is still there.

```bash
cd packages/pi-legion/scripts/skill-scenarios
LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic ./rig.sh profile head <checkout>
# or, with no gateway: LEGION_E2E_MODEL_ROUTE=key-file LEGION_E2E_ANTHROPIC_KEY_FILE=<0600 file>
# or, through a provider block of your own models.yml whose apiKey is `!cat <absolute file>`:
#   LEGION_E2E_MODEL_ROUTE=provider LEGION_E2E_MODEL_PROVIDER_FILE=<models.yml> LEGION_E2E_MODEL_PROVIDER=<block>
./rig.sh batch ask-on-message 5 head base   # starts and stops Postgres, NATS, one listener per label, and each run's own scratch Dispatch
./rig.sh batch tester-proof 5 head base
./rig.sh score --arm ask-on-message:head=5 --arm ask-on-message:base=5 \
  --arm tester-proof:head=5 --arm tester-proof:base=5
```

A batch numbers each label's runs from one above that label's highest run of the scenario (and
case), live or archived, so `rig.sh batch ask-on-message 1 head` twice gives the runs named
`ask-on-message-head-1` and then `ask-on-message-head-2`. A run's name is in its record and
nowhere its agents can read: its directory is `runs/<id>`, `<id>` twelve hex digits of no meaning,
and its record is `runs/<id>.json` beside it, outside the run's namespace:

```json
{"run":"<id>","scenario":"threads-closed","case":"correct","label":"head","n":1,
 "name":"threads-closed-correct-head-1","batch":"<tag>-<start>","ran":"<work>/runs/<id>"}
```

`case` is `""` for a scenario that takes none, `mode` is there for an askgate run given one, and
`ran` is the directory the run had while it ran, which its transcripts name. The record moves with
the run to `archive/runs/<id>.json`. `rig.sh score`, `rig.sh health`, `rig.sh judge` and
`rig.sh world` read a run's scenario, case and label from it (`run-names.ts`'s `runIdentity`), and
a run recorded before the record existed from its directory's name and its `case.txt`, as before.
The labels' profiles are installed under `installs/<id>` (`profiles/<label>/install` holds the id),
and every label's profile is named `legion`, so neither a path nor `OMP_PROFILE` names a label.

The spec-writing and spec-rewrite scenarios measure how an agent writes a Dispatch spec:

```bash
bun fetch-spec-sources.ts --out "$SKILL_SCENARIOS_WORK/sources" <issue key | dispatch://PROJECT/artifact/slug>
SKILL_SCENARIOS_SUPERPOWERS=<superpowers plugin> ./rig.sh profile main <checkout>
SKILL_SCENARIOS_SOURCE=<directory under sources/> ./rig.sh batch spec-writing 1 main
./rig.sh judge "$SKILL_SCENARIOS_WORK"/archive/runs/<id>   # the run whose record names spec-writing-main-<n>
./rig.sh score --arm spec-writing:main=1 spec-writing
```

The scenarios and every input are in the script's header (`./rig.sh` with no arguments prints it),
and each scenario's scoring rule is on the `score.ts` function that scores it. It needs a model
route (the gateway, an Anthropic key file, or a provider block of the operator's own models.yml).
A batch needs Docker only where it starts the Postgres and NATS containers. Where both
`SKILL_SCENARIOS_POSTGRES_URL` and `SKILL_SCENARIOS_NATS_URL` are set, as in a Legion pod, which has
no Docker, the batch uses those two servers, starts no container, and needs `psql` and `python3`
instead, to read the Postgres URL with libpq and to reach the server. `SKILL_SCENARIOS_POSTGRES_URL`
must be a `postgres://` or `postgresql://` URL of the Postgres on this machine. Each run's Dispatch
gets the connection string libpq reads of it, for the run's own database (`run_database_url`), and
the two readers that open that database with Bun.SQL, `seed.ts pull-request` and the head watcher,
get that string as a `postgres://` URL with each value percent-encoded (`rig.sh`'s `conninfo_url`),
never the given URL's own text, of which Bun can read another host than libpq does.
A judge runs in a Docker container, unless both service URLs are given, when it runs in the pod
(below). Every container a batch or a judge starts has CPU and memory limits. It runs a mise of
its own, never the caller's, since a worker pod's image carries none (the image's `tools` stage
alone installs mise): `rig.sh profile`, `live-read`, `batch` and `judge-omp` fetch it with `curl`
at the worker image's own pin, `packages/daemon/docker/worker.Dockerfile`'s `ARG MISE_VERSION` and
`ARG MISE_SHA256`, read there on every call, from the release URL that image's `tools` stage
fetches, install it as `$SKILL_SCENARIOS_WORK/tools/bin/mise`, and put that directory first on the
PATH the profile installer and every agent run on. A fetch that fails, or an archive whose SHA-256
is any other, is refused, naming the step and what it expected and got, and installs nothing; a
later call reuses the mise an earlier one installed from that archive.
Every run's agent starts in a detached tmux session, and the rig brings no tmux of its own:
`rig.sh batch` and `rig.sh run` first run `tmux -V` and start and kill one detached session on a
socket of their own under the work directory, and refuse, before they make any database, service
or run directory, naming `tmux`, the step that failed and what tmux printed on stderr. A batch's
runs take its PATH, and so the tmux it checked, and are not checked again. A worker pod's image
carries tmux, trixie's package (`packages/daemon/docker/worker.Dockerfile`'s apt list), which
answered `tmux 3.5a` on 2026-11-25.
`SKILL_SCENARIOS_WORK` must have no `.omp` in it or above it, so not under your home directory: Oh
My Pi would load that directory's plugins and not the label's. Its bounded waits, port picks and
process stops are
[`scripts/e2e/lib/rig.sh`](../../../scripts/e2e/README.md#librigsh)'s. Nothing a batch runs reaches
production Dispatch, GitHub, Envoy or NATS: it starts nothing with the caller's `DISPATCH_*`,
`ENVOY_*` or `NATS_*` variables, and `seed.ts` refuses to run while `PLAYWRIGHT_BASE_URL` or
`E2E_AGENT_TOKEN` is set. For NATS this holds only when the batch starts its own NATS, or when
`SKILL_SCENARIOS_NATS_URL` names a scratch server of the batch's own: the listener and every
tester-proof run's agent get that URL. That is the environment only: an agent reads the whole
filesystem, the other label's checkout included, so `score.ts` does not score a run that read
outside its own label. A judge is held tighter: `rig.sh judge-omp` runs each one in a Docker
container that sees only its own directory, read-only but for its one answer file and its
transcript, so a judge cannot read the run's transcript or another judge's input. Where both
`SKILL_SCENARIOS_POSTGRES_URL` and `SKILL_SCENARIOS_NATS_URL` are given, as in a Legion pod, which
has no Docker, the judge runs in the pod instead, in a namespace of its own built as a run's
agent's is (below), under `env -i`, with a HOME and a `/tmp` of its own beside its directory, and
sees its directory, its key directory and its Oh My Pi alone. There its directory is read-write
as a whole, since the pod's namespace cannot mount onto a single file, where the container makes
only the answer file and `sessions/` writable; `judge.ts` reads the one answer file and nothing
else of the directory. Its CPU and memory are bound by the pod's alone. On the key-file
route the container reads the key file itself. On the gateway route the gateway's key command runs
on the host, once for each judge, because it needs the operator's home and session bus. The
container gets only the token, from an owner-only file that is removed when the judge ends. On the
provider route the profile's models.yml is the block as the operator wrote it, `!cat` command
included, so no key is copied anywhere: the container mounts the directory of the file that
command reads, read-only.

Each run's agent runs in a user, mount and PID namespace of its own (`env -i
PATH=/usr/sbin:/usr/bin:/sbin:/bin unshare -Urm -p --fork --mount-proc`, then a user namespace that
maps it back to the caller's uid), built from one declaration in `rig.sh`, `world_paths`, which
`rig.sh world <label> <run dir>` prints. It hides behind an empty tmpfs the scratch root the
caller's `TMPDIR` names, the issue workspaces (`/legion/workspaces`, or `LEGION_WORKSPACE` three
levels up), the work directory, every label's checkout, `/var/run/legion` and `/var/run/secrets`;
it shows again the run's own directory, its label's install (never the other label's), the
batch's two tokens (`services/tokens/`), and the tools the run's pane names (each directory on its
PATH, the rig's own mise's first, mise's installs, the pin and the push guard's check), read-only
but for the run and its model route's call record. No checkout is shown, the label's own included,
and not the rig's own directory either: the run's `gh` is a bundle of `gh-standin.ts` in
`<run>/lib/gh.js`, which names the run it records under itself. `/tmp` is the run's own
`<run>/tmp`, and `/proc` lists the run's own processes alone. `/var/run/model-gateway-key` and
`/var/run/operator` stay: they are the model route's own credentials. So an agent can neither list
nor read another run, another role's scratch, the other label's skills or the pod's credentials,
nor leave a file in `/tmp` for another run. A run recorded before this world is read against the
world it had (its own `namespace.txt`, or, with none, a world that showed its checkout, the rig's
directory and the services).

The world shows the system configuration of the pane's git (the file `git config --system --edit`
names) as the host holds it, at its own path and read-only (`world_paths` declares it `ro`), with
no mount onto it, and no file where the host has none; each run's `services-from` records its
digest, or `none` (`system-gitconfig`, below). The rig runs in the deployment's own pod, so that
file is the one the production pane reads, for any deployment and any image. INFRA's worker image
of 2026-11-19, for one (`worker-infra@sha256:c2b4e9b7…`, that pod's 11:51Z update, read that day),
carries `/etc/gitconfig`, 57 bytes, sha256 `0c8ac2d6411b87ec…`, holding
`core.packedGitWindowSize = 32m` and `core.packedGitLimit = 256m`, which that pod's git lists as
`system core.packedgitwindowsize=32m` and `system core.packedgitlimit=256m`. The world used to
declare the file `empty` and bind an empty read-only file over it, on the premise that the worker
image had no `/etc/gitconfig`: that hid those two lines from the agents, and a gVisor pod refuses
a mount onto a file (`mount point is not a directory`), so the probe below refused that pod and
every run on it. A run off a deployment's pod shows that host's system configuration, which is
then not production's (a GitHub runner's holds `safe.directory=*` and four `filter.lfs` keys):
that is a limit of running off the pod. `rig.sh health` calls unhealthy the runs of one batch
that record two system configurations, since a batch's runs share one pod.

Nothing the rig composes for an agent names its run's scenario, case or label, or a name the rig
once gave its world: before an agent starts, `launch` runs `run-names.ts guard`, which reads every
variable of the pane (its name and its value: the profile, the Legion project, issue and role its
Envoy topics are made of, HOME, TMPDIR, PATH), every mount of the namespace (each is a mount point
and the host source `/proc/self/mountinfo` names, so the work directory's own path is held too),
the working and session directories, the system prompt (`/proc/1`'s command line and the file it
names), and everything of the run's directory the rig wrote, by its path and its text, one file at
a time: the rig's records, the stand-ins in `bin/`, what the rig builds (the `gh` bundle
`lib/gh.js`), the pane, the prompt, the files `rig.sh profile` writes into the HOME the run copies
from the label's install, the seeds' ids, the fixtures and the world's repositories. The run fails,
naming the word and the place with the text around it, where one is there. One walker reads a
run's files (`run-names.ts`'s `worldFiles`), and at every launch it passes over what an agent
wrote, whose words are its own (`AGENTS_OWN`, each entry with why):

- a message one agent of the run sent the next, which `run_threads_closed` relays into the
  reviewer's prompt and keeps whole in `<run>/reviewer-wake.json` (the architect's ruling of
  2026-11-20T17:30Z): the guard reads the prompt less each such message, at the end of the line
  the rig wrote it on, so the rig's own prefix of that line is still read and the message's words
  are its sender's (`run-names.ts`'s `promptOf`); a `reviewer-wake.json` it cannot parse fails the
  run. In INFRA-764's round 26 the implementer's "Pushed one text-only commit" refused its
  reviewer's launch;
- each agent's transcripts (`sessions/`), output (`out*.txt`, `boot-1/`), the HOME an earlier agent
  used (`home-<role>/`), its TMPDIR (`tmp/`), and the logs of what the agents did (`logs/`,
  `calls.jsonl`, `published.jsonl`, `daemon.log`, `pushes.log`): read before a run's second
  agent, the first agent's transcript refused it;
- a spec run's asks as Dispatch holds them after a turn (`state-<turn>.json`) and the person
  stand-in's answers to them (`answers*.jsonl`);
- the repositories' version-control records (`.jj/`, `.git/`, and `remote.git` but its hooks),
  which keep each agent's commands, commits and branch names as it makes them; the trees,
  histories and hooks the rig builds are held by `rig.test.ts`'s "what the rig writes into a run's
  world" for every world `rig.sh` builds.

What an agent of the run can write is read at the run's first launch alone (`AGENT_WRITTEN`, each
entry with the write that reaches it; the architect's ruling of 2026-11-21T14:24:55Z): its working
directory (`cwd/`), the issue workspace (`ws/`) and the clone it is a worktree of (`repos/`), its
HOME (`home/`), a worker pane's XDG directories (`state/home/`) and the Legion plugin's files in
its state directory, and the gh answers (`fixtures.json`), which the gh stand-in writes again on
an agent's edit of the pull request's body. Before the first launch they hold only what the rig
wrote, and the guard reads them. At every later launch (boot_agent's relaunch, threads-closed's
reviewer, a spec run's later turn) they hold what an agent wrote too, and the guard reads every
other file of the run. `launch` tells the first from a later one by the run's record of its
launches, `<run>/logs/launches`, a line `<time> <tmux session>` it appends once the guard has
passed and before the agent starts. `<run>/logs/` is under an empty read-only tmpfs in every
agent's namespace, and a namespace of the agent's own cannot take that mount off either, so only
the rig writes the record (`rig.test.ts`'s "an agent of the run cannot write the run's record of
its launches"). A run with no line there is at its first launch (`run-names.ts`'s
`firstLaunch`). The rig writes nothing into those paths after the first launch:
`rig.test.ts`'s "after a run's first launch the rig writes nothing into what an agent of the run
can write" composes every worker scenario's case and both spec scenarios past their first agent,
with an agent that writes nothing, to each later launch they reach (every relaunch, the
reviewer of `correct`, a spec run's second and third turns), and holds each such file to the
bytes the first launch read. The gh stand-in is rig code too: `gh-standin.test.ts`'s "a write the
gh stand-in keeps in fixtures.json adds nothing the call did not give it" holds each body edit it
models, and the review writes, to strings the call gave or the file already held. The rig writes
no agent's words into a file the guard reads either: `seed.ts state` writes a spec run's open
asks to `state-<turn>.json` and prints only their count into `fixture.log`, where the agent's ask
would otherwise refuse its next turn.

The guard reads what the rig wrote for the run, never what the label's checkout or the host
provides (the architect's ruling of 2026-11-21T17:22:22Z). Of the HOME `base_env` copies from the
label's install (`cp -a "$I/home" "$R/home"`) it reads the files `rig.sh profile` writes there
alone (`RIG_HOME`, each entry with its write): the profile's settings and model route
(`agent/config.yml` and `agent/models.yml`, from `install_model_route`'s installer), the Envoy
configuration `--envoy-json` merges (`agent/envoy.json`), the superpowers skills
(`install_superpowers`) and `make_omp_home`'s jj overlay (`.jjconfig-operator-paths.toml`). The
label's packed plugins under `.omp/profiles/<profile>/plugins/` are the label's (`LABELS_OWN`, a
third list beside `AGENTS_OWN` and `AGENT_WRITTEN`), and no other file of that HOME is read
either, since no write of the rig's makes it. The walker enters no link: it lists each directory
by `lstat`, reads the path each link names for the guard's words, and judges where that path is.
A path inside the run is read as itself and no further. One inside the label's
install (`<work>/installs/<id>/`, the install `load_label` records, which `launch` passes to the
guard), the host's native module cache (where `make_omp_home` links each install's
`.omp/natives`, the directory `omp-home.sh`'s `omp_natives_dir` prints:
`$XDG_CACHE_HOME/omp/natives` where `XDG_CACHE_HOME` is set and `$XDG_CACHE_HOME/omp` exists,
else `~/.omp/natives`) or the host's git exec path (`git --exec-path`, whose programs `world_remote`
links into `<run>/lib/git-core`) is not read. One anywhere else refuses the launch, naming the
link. The guard holds one file's text at a time. In INFRA-764's round 31 at `c42409bc` the walker
went through the HOME's links, since Bun's recursive `readdirSync` enters a link to a directory:
it refused every run on `var SPEC_WRITING_POINTER` in `pi-envoy/dist/dispatch.js`, and with the
host's natives (124 MiB) read whole it reached 1,170 MiB, where the pod's memory guard killed it
twice. `rig.test.ts`'s "the guard opens nothing a link of the run points to outside it, and reads
nothing of the run's HOME the rig did not write" holds a link of each kind to the access times of
the trees it points into, and its "a link of the run to a file outside it and the trees the label
and the host provide refuses the launch, naming the link" holds the refusal.

`services-from` names the rig's own directory in every run, and the walker reads it less that
directory (the residual below). `rig.test.ts`'s "no file rig.sh writes, copies or builds into a
run before its agent starts names a word of the measurement" holds the same walker to every world
the rig composes, as it reads it at the first launch. It sources `rig.sh` whole, brings a batch's
services up with `services_up` (the listener, the Dispatch server, `seed.ts` and the processes a
run starts beside its agent stubbed; the label's build of `daemon-pane.go`, which composes the
pane and its system prompt, built), and runs `cmd_run` for each scenario and case of `SCENARIOS`
and both labels, both built from the test's own checkout, each on a real install made as `rig.sh
profile` makes it (`make_omp_home`, then `install-plugin-profile.sh` for both plugins, which packs
each and installs it with the pinned Oh My Pi through the caller's mise, then the provider model
route on a provider file of the row's own, since the other routes need credentials no test holds),
so each run's HOME holds the plugins' links and the natives link a batch's does, under
`SKILL_SCENARIOS_COMPOSE_ONLY`, which ends a run once `launch`'s guard has read its world, records
no launch and starts no agent; then it reads every file of each run through `worldFiles`, names
what the walker met of the HOME, and holds the guard's own peak (`ru_maxrss`), as `launch` ran it
in each run, under 256 MiB. In a Legion pod, where a bare `bun -e 1` peaks at 139 MiB, the guard
peaked at 167.9 to 193.5 MiB over the row's 38 runs and at 182.6 MiB over a copy of round 31's
run `4f4d19499cf8`, where it reached 1,170 MiB at `c42409bc`. The `gh` bundle holds every
module `gh-standin.ts` imports, so the evidence of the world's routes, which names recorded runs
by the names they ran under, is in a module the stand-in does not import
(`gh-route-evidence.ts`'s `ROUTE_EVIDENCE`), and the stand-in reads `gh-routes.ts`'s routes and
answers alone: at `9083afb5` the stand-in came to import the route table with its evidence, and the
guard refused every reviewer of INFRA-764's round 30 on the scenario's name in `<run>/lib/gh.js`.

The services' logs (`<run>/logs/`) are hidden from the agent under an empty read-only tmpfs. The
rig's records that are files at the top of the run's directory (`built-from`, `services-from`,
`namespace.txt`, `launch.sh` and the rest) are not hidden, since a gVisor pod refuses a mount onto
a file: an agent can read them, the guard reads them, and hiding them needs them moved into a
directory the namespace covers. The words are derived, never typed: every scenario and case of `run-names.ts`'s
`SCENARIOS`, the rig's one scenario table, every label, and `REMOVED_NAMES` (`rig-`,
`skill-scenario`, `lweval`, `e2e-seed`, `example/widgets`). So the work directory's default is
`$TMPDIR/work`, the world's repository is `fernbrook/widgets`, its Dispatch project is `WID`, the
seeded sessions have ids shaped as any role's session, and the Legion project is the repository's
name and a digest. What stays a rig path: the work directory's layout (`<work>/runs/<id>/home`,
`<work>/installs/<id>`) where a Legion worker has `/home/<user>`, `/legion/workspaces/<owner>/
<repo>/<key>` and the issue volume's `TMPDIR`. Moving them there would have `rig.sh` rewrite the
pane the label's daemon composes, `health.ts`'s world and credentials rules accept those paths
under the covers they lie in, and `score.ts` read them as the run's own; that is a change of its
own, so the paths are neutral in their words and not production's in their shape.

The guard reads a text with each underscore as a hyphen, and a case standing inside a longer
identifier joined by `_` or `-` is that identifier's and not the rig's word (the architect's
ruling D6 of INFRA-764's plan v9, `run-names.ts`'s `besideWord`, which health's names rule reads
too). A reviewer's system prompt and every world's base rules name `require_last_push_approval`,
which holds the case `last-push`, so a guard that read the case there would refuse every worker
run; `last-push` standing alone still refuses (`rig.test.ts`'s planted-word row, and its row of a
reviewer whose system prompt names the rule).

A host that refuses the namespace starts
nothing: `rig.sh run`, `batch` and `live-read` probe it first and exit 1 naming the host's refusal
(a GitHub runner answers `unshare: write failed /proc/self/uid_map: Operation not permitted`), and
the rig's namespace tests skip there with that text. When a run ends, the rig moves its directory
from `$SKILL_SCENARIOS_WORK/runs/<id>` to `$SKILL_SCENARIOS_WORK/archive/runs/<id>`, its record
with it;
`rig.sh score` reads both directories, and `rig.sh judge` and `rig.sh health` take a run in
either. No run removes or replaces another: a run whose name a live or archived run's record (or,
for a run from before the records, its directory) holds is refused before it starts, and a run
whose id the archive took while it ran is refused at its end, naming both paths, and stays in
`runs/`. The rig deletes only what the same run or command
made, and refuses a file an earlier one left where it would write: a batch refuses the services
files a batch killed outright left (each label's `services/env-<label>` and
`services/services-from-<label>`, its tokens under `services/tokens/` and each listener's
`listener_<label>.port`) and
removes only the containers it started; `judge-omp` refuses a directory
holding an earlier judge's `out.txt` or `judge-model.txt`, or the key directory beside it; a
judge-profile build an earlier call left unready is refused until removed; `rig.sh judge`
(`judge.ts`) refuses, before any judge starts, a run whose judge directories under
`$SKILL_SCENARIOS_WORK/judging/<run>/` or whose answer files (`answers-<turn>.jsonl`; `faults.json`
to `shared-runs.json` for a spec run; `accepts.json`, `names.json` and `testing.json` for a worker
run) an earlier judging left, naming each, so a run is judged again once those are moved aside;
and `seed.ts capture-spec` refuses a run directory that already
holds what it writes (`spec.md`, `plan.md`, `documents/` and the rest). The one exception is the
gateway key command's cached token, which a judge drops to mint one that outlives it. At
`SKILL_SCENARIOS_PARALLEL=1` a run's exit also records what appeared at the top of the machine's
`/tmp` while it ran, by the entries it recorded there when it started, and deletes none of it. A
run's pushes meet the push guard a Legion pod's do: where the `git` on the
caller's PATH is Legion's leak-check wrapper, the rig passes the caller's `PUBLIC_LEAK_CHECK` into
each run's pane and refuses to start a run when it names no readable check. In a worker world the
pane names the world's run of that check instead (below).

A worker world's issue workspace, `<run>/ws`, is made as production's provisioning makes an issue
pod's (`rig.sh`'s `fixture_workspace`): a shared clone, `<run>/repos/github.com/<owner>/<repo>`,
cloned with no working tree from the run's own bare remote as a pod's is from its feed, and the
workspace added to it with `jj workspace add` at the issue branch's head once the world's last
push is made (the reviewer's text commit, in `threads-closed`'s accept, correct and crossing), so
its `@` is an empty change on that head. jj colocates it as a locked git worktree, so
`<run>/ws/.git` is the file a pod's is (`gitdir: <clone>/.git/worktrees/ws`). Both of the
worktree's links are absolute and the clone's format is 0 with no extension, as a pod's git
2.47.3 writes them, whatever the host's git. jj asks git for relative links (`git -c
worktree.useRelativePaths=true worktree add`); git 2.48 and later honour that, and write
`gitdir: ../repos/…`, `core.repositoryformatversion=1` and `extensions.relativeworktrees=true`
(CI's git 2.56.1 at `427b02d4`; git 2.55.0 measured here). So the build runs `git worktree
repair` with `worktree.useRelativePaths=false`, unsets the extension and sets the format back
to 0, and refuses a workspace in any other form, naming each difference (`workspace_form`).
Its origin is `https://github.com/<owner>/<repo>`, as a pod's is, and `git remote -v` and `jj git
remote list` print that URL. Every fetch and push of origin by its name, git's and jj's, still reaches
`<run>/remote.git`: origin names a remote helper (`remote.origin.vcs=ghapp`), and git runs
`<run>/bin/git-remote-ghapp` from the pane's PATH, which hands the call to `git remote-ext` and so
to the bare remote's own upload-pack or receive-pack, with none of the caller's repository
variables (`git rev-parse --local-env-vars`), as git's own local transport hands it none. Measured
on 2026-11-18 (git 2.47.3, jj 0.45.1): the pane's `core.hooksPath=/dev/null` reached
git-receive-pack through remote-ext until the helper dropped them, and the remote's post-receive,
which writes `pushes.log`, never ran. `url.<local>.insteadOf` was ruled out: `git remote -v` and
`jj git remote list` print the rewritten local path. The clone's configuration holds a pod's keys in
a pod's order (the credential lines, `core.warnAmbiguousRefs`, `branch.main.*`; none of the
`core.ignorecase` and `core.precomposeunicode` that `jj git clone` wrote) and the pane carries the
five `GIT_CONFIG_COUNT` pairs a pod's runtime gives every role process (`rig.sh`'s `git_pairs`), so
`git config -l --show-scope` prints a pod's list and `remote.origin.vcs`, which `rig.test.ts`'s
"the issue workspace's remote" holds line for line. That row holds the `system` scope to the host's
own file's lines, read in the agent's namespace where the host makes one, and outside it on a host
that makes none (a GitHub runner), where no agent starts; its name says which. The clone and the
workspace get the rest a pod's provisioning writes too: `.jj/legion-created`, the `.codegraph/`
line of the clone's `.git/info/exclude`, and the repository's jj configuration (`trunk()` and
`git.abandon-unreachable-commits = false`). jj 0.45 keeps that configuration under the
configuration home of whoever runs jj (`$XDG_CONFIG_HOME/jj/repos/<config-id>/`), and the rig's
jj wrote it into the operator's, so every agent's first jj command printed `Per-repo config not
found. Generating an empty one.`, which a pod's roles never see (all 35 runs recorded on
2026-11-17). Every jj of the world's build and of `adopt_workspace` now runs with the
configuration home a worker pane gets, `<run>/state/home/.config`, so the agents' jj reads what a
pod's reads and the operator's directory is not written.

The helper takes a call of origin by its name, and nothing else: a git or jj call that names the
URL itself (`git ls-remote https://github.com/<owner>/<repo>`, `git clone` of it) reaches the live
github.com, as it did before origin named it, and health calls the run off its world. The shared
leak check makes such a call on every push: its `git ls-remote -- <the push's URL>` reached
github.com, was refused, asked `git credential fill` and so the pane's `legion git-credential`,
and sent the world's token to github.com (measured: two connections, one with `Authorization:
Basic`). It deletes every `GIT_CONFIG*` variable before its own git, so a pair cannot route it.
So the pane's `PUBLIC_LEAK_CHECK` names the world's run of the check, `<run>/lib/public-leak-check.sh`,
which runs the caller's check (named in `<run>/lib/leak-check`, whose directory the namespace
shows) with `GIT_EXEC_PATH` at `<run>/lib/git-core`, for that process alone: a link to every
program of git's own exec path but `git-remote-https`, which sends the world's own URL to the same
helper and every other URL to git's own. Measured at a push of a real world: no connection to
github.com, no credential asked, the check passed a clean push, and a planted private host was
refused and never pushed. The guard's read of the repository's visibility goes to the run's `gh`,
which answers it from the world's repository (`public`), and `calls.jsonl` records that read as a
call of the pushing role. No rule counts it: each reader of the record takes a call by what it
does (`score.ts`: a review, a reply, a thread's resolve or listing, a body it wrote, the billing
read, a `bun` run; `health.ts`: a refused call, a write), and a GET of the repository that exits 0
with no body is none of them.

The remote answers each push as GitHub answers the App that makes it. Production's review App
(architect, planner, tester, reviewer) holds `contents: write` and no `workflows` permission, and
the implement App (implementer, merger) holds `workflows: write`
(`packages/daemon/internal/appauth/permissions.go`'s `writePermissions`,
`scripts/github-apps/review.json` and `implement.json`). GitHub refuses a push of an App without
`workflows` whose commits create, update or delete a file under `.github/workflows/`, and the
world's remote does too (`rig.sh`'s `fixture_first_commit`, in the same `proc-receive` hook as the
base's ruleset): `git push` prints ` ! [remote rejected] <src> -> <dst> (refusing to allow a GitHub
App to create or update workflow `<path>` without `workflows` permission)` with no `remote:` line
and exits 1, `jj git push` prints `Warning: The remote rejected the following updates:` and the ref
with the same reason and exits 1, and the ref does not move and `pushes.log` gets no line. The
reason is GitHub's, as a reviewer's refused push printed it
(`docs/solutions/github/moving-a-package-between-jobs-moves-its-reasons-too-and-only-the-implementer-pushes-a-workflow-file-pr2516.md`,
and the LEGION-779 page it extends). A path counts where a commit of the push that is on no ref of
the remote yet creates, changes or deletes it, and its content there is on no branch the remote
holds, GitHub's documented exception for "the same file (with both the same path and contents)"
on another branch (Scopes for OAuth apps, `workflow`); a merge that takes one side's version of
the path counts for nothing. A push that breaks both this rule and the base's ruleset prints the
workflow line alone. Unmeasured: a deletion pushed (GitHub asks `workflows` of a workflow file's
`DELETE` through the contents API, Permissions required for GitHub Apps), which path the line
names when a push touches several (the world names the first in byte order), and that order of
the two refusals.

The remote reads the App off the transport, as GitHub reads it off the push's token. The issue
workspace's helper exports `GITHUB_APP`, the slug of the App it pushes as, over whatever the
caller's environment holds (`world_remote_app`): `world_remote` writes it for the implement App,
the App the workspace is provisioned with, and `worker_boot` writes it for each role before that
role's agent starts. Measured on 2026-11-19 with a hook that wrote what reached it, at a `git push
origin` and a `jj git push` from a reviewer's pane: the helper ran for both, no credential was
asked, and the pane's environment arrived whole but for the repository variables, `LEGION_ROLE`
and the identity variables among it. Those variables are the agent's to set for one command,
which would give a reviewer the implement App's push with no credential at all, and the
credential reaches the remote only if something reads the token, which no stand-in does; so
the App is the transport's own, written per role as the daemon renders a role's gh bundle at the
pane's spawn. A push that names `<run>/remote.git` itself carries no App and is taken, as the
world's build's own pushes are. A refused push is no push to the readers: `score.ts`'s
`pushesOf` reads `pushes.log`, which `tester-proof` (its pushes and their tip),
`reviewer-text-findings` (the text-only case's one push of the reviewer's text commit, and the
defect and last-push cases' none) and `threads-closed` (the pushes onto the issue branch and the
diff of the last, `fixed`, and the accept, crossing and code-defect cases' no push) count
from; and `health.ts` reads `pushes.log` and the branch's tip for rule 19 (`headUnmoved`) and a
tool result for the leak check's own refusals alone for rule 6 (`GUARD_REFUSAL`), which no
` ! [remote rejected]` line is.

A phase worker whose claim boot is refused before its first turn is launched once more, in the
same run, as production's daemon relaunches an agent whose process exits at its boot: the daemon
stand-in refused its `claims/register`, or its plugin printed its sentence for a claim boot that
registered and could not finish and exited, read in the wording of the run's own label: the
opening is read out of `packages/pi-shared/src/legion-claim-core.ts` in the checkout the run's
`built-from` names (this checkout's `claim boot failed at <step> …`, an older plugin's, main
`ff2a10ef`'s, `claim boot failed after claims/register registered …`). Round 23 lost
`reviewer-text-findings-text-only-base-1` so, to the Envoy listener's refusal of a session that
held no consumer. `boot_agent` runs `boot-refusal.ts` itself to decide it from what the run
recorded (`health.ts --boot-refusal` answers the same, for a replay through the old entry);
`boot-refusal.ts` is the one reader of a claim boot refusal, and loads neither `health.ts` nor
`score.ts`: the stand-in's lines of that role since the launch began, the pane's `out.txt`, and
the agent's transcripts, which must hold no turn, no model call and no tool call. The first
attempt is kept whole under `<run>/boot-1/<role>/` (its `out.txt`, its `sessions/` and the
stand-in's lines of it), and the second attempt's record stands where any agent's does, so
`rig.sh score` and `rig.sh health` read the run on it. The stand-in, its boot token and the
agent's HOME carry over, as a pod's do. The rig relaunches once, where the daemon's launch budget
relaunches an agent up to
its launch failure limit: a second refusal stands, and one after the agent's first turn is never
relaunched, since a relaunch would not take that turn back. Each attempt is on the run's boot
record, `<run>/booted`, before it starts: worker_boot writes `boot <role>` before the role's daemon
stand-in starts and the relaunch writes it again, each launch `boot_agent` asks the reader about
is followed by `since <role> <time>`, the `since` it asked with, so a replay asks the reader with
the call's own argument, and where the rig decides not to boot a role of its scenario it writes
`skip <role> <reason>`, with the one reason `completion-answers.ts`'s `SKIPS` declares for the
scenario and case (today, threads-closed's reviewer in accept, "the accept case scores the
implementer's answer alone", in crossing, "the crossing case scores the implementer's answer
alone", and in code-defect, where the implementer sent the reviewer no message, "the implementer
sent the reviewer nothing, which fails the message the case asks"), read from that table.

`rig.sh health [--tmp-snapshot <file>] [--repo <git dir>]... <run dir>...` says whether each run's world could take what
its agents did, and whether the run stayed inside its own world. It reads twenty-three things: the
run finished: its boot record, `<run>/booted`, decides each role its scenario boots
(`completion-answers.ts`'s `ANSWERS` `boots`) once, in that order, the first a `boot` of the first
role and each other a `boot <role>` or a `skip <role> <reason>` whose reason
`completion-answers.ts`'s `SKIPS` declares for the run's scenario and case (a relaunch's repeated
`boot` line is one decision), and each role it booted ended on its own record: its own output ends
with an `exit=` line (`out-<role>.txt`, or `out.txt` for the last role booted) and its own session
directory (`sessions/` for the first role, `sessions/<role>/` for each later one, a subagent's
transcript never) holds a transcript with an entry; a run with no `booted` is unhealthy; the run
carries its `services-from`, and every service of its label's own (scope `label`) came from the
checkout its `built-from` names, the listener, the Dispatch server, the daemon facts and the
worker pane among them (below); no agent's claim boot was
refused but a first attempt's before its first turn, which the rig relaunched (a run refused on
both attempts is unhealthy, naming both, and so is one refused after a turn); the runs given of
one batch (each run's record's `batch`) record one system git configuration in their
`services-from` (`system-gitconfig`), since a batch's runs share one pod (a run with no record, or
one recorded before that line, is compared with none); every completion
line names a role the scenario boots, and every completion the daemon stand-in refused was
answered as `completion-answers.ts`'s `ANSWERS` table allows for the run's scenario and that
role: an answer a daemon of either shape gives it, or one the daemon of the run's own label gives
it, read out of the sources of the commit the run's `built-from` records (below; a run with such
an answer whose daemon cannot be read is unhealthy); a reviewer-text-findings world of the `defect`
case took the reviewer's completion at least once (`completion-answers.ts`'s `ANSWERS` `takes`,
keyed by case), and one of the `text-only` or `last-push` case need take none, since a reviewer that
waits for the implementer completes nothing: a text-only reviewer that pushed its own text fix
records no pass at it and approves nothing while it waits; no
request the daemon stand-in answered 404 is of a route the run's label's daemon registers, read
out of the source of
that same commit (the stand-in answers by the same routes, and a run with
a 404 whose routes cannot be read is unhealthy); no turn outcome the stand-in answered failed the
claim, as the daemon fails one whose provider refused turns up to its launch failure limit; no
tool result of any
agent, and no `out*.txt`, holds a line the push guard prints when it refuses a push or cannot run;
no `gh` call the gh stand-in refused (`calls.jsonl`) with a refusal `health.ts`'s `GH_DECLARED`
does not declare for the scenario and the calling role (a call no route of the world answers
never is, nor a read the world refused as one it does not model, recorded `unrouted`; the
refusals production gives for a workflow run the world holds none of, GitHub's 404 for its jobs
and gh's for `gh run view` and `gh run watch`, are, for every role, and so are gh's refusals of a
pull request, a branch, a repository or a `--json` field GitHub or gh has not got, the
Legion gh wrapper's refusal, exit 2, of a call naming a repository whose owner is outside the
project, and the world's 404 to a read of such an owner's repository that the label's wrapper runs
on the pane's own token (`OWNER_UNHELD`, below); the world answers
every `gh` read either label's role texts name, which `gh-routes.ts`'s `WORLD_ROUTES` lists, each
route no text names with its evidence in `gh-route-evidence.ts`); no `gh` write (a GraphQL mutation,
a `gh api` call sent
with a verb other than GET, a `gh pr`, `gh run` or `gh auth` command that writes) went unanswered by
every route and answer the world models: one the gh stand-in recorded as answered by nothing
(`handler` `unrouted` in `calls.jsonl`) or, in a record from before it kept `handler`, one the
catch-all write route (`^gh api `) answered a success that changed nothing, which filed no review
thread comment and was no review submit; no request of an agent that the run's own Dispatch server
answered with an error, 4xx or 5xx, by the server's own line of it in `<run>/logs/dispatch.log`,
unless `health.ts`'s `DISPATCH_DECLARED` declares that answer for the scenario and the agent's role,
or it is Dispatch's 403 `OAUTH_SCOPE_REQUIRED` to a route the run's label's Dispatch gives the
`person` scope (`routes_table.go`'s `routeScopes`) and its agents' client calls
(`dispatch-http.ts`), which production gives every session of the shared agent token, declared
for every agent and read out of the run's own commit by `person-routes.ts`'s `labelPersonRoutes`,
so `dispatch send-to-agent`, `dispatch inbox` and `dispatch review` among them and any such route
main adds, or it is a 400 to `POST /api/v1/issues/{key}/results` that the agent's own transcript
shows as `RESULT_INPUT`, which production's Dispatch answers the same input (a commit short of 40
hex characters), and whose next record of that result Dispatch took (`correctedInput`: the agent's
last `dispatch result` of that issue before the request, the one that made it, has a result
naming that code, and the next request to that route and issue is the agent's, answered 2xx, after
its next `dispatch result` of that issue, for the same check; a 400 its transcript shows as
`UNKNOWN_CHECK`, or as no code, or one never recorded again is unhealthy); a 403 of a run whose
label's routes cannot be read so is unhealthy (the agent booted
last before the request began, by the opening line each daemon stand-in writes before its agent
starts; `HANDOFF_NOT_FOUND` for the `review` handoff is declared for reviewer-text-findings'
reviewer; a run
with no such log is unhealthy); the run's GitHub stand-in answered as GitHub does, by its own log,
`<run>/logs/github.log` (no request it refused or holds no route of, no status GitHub does not
give, and a worker world's log kept); every push of the pull request's branch was stored as
Dispatch's head, by the head watcher's `<run>/logs/heads.log`, and the branch stands at a head
the record stored; no tool call names a path under another run's directory;
no tool call reads a path another of the runs given wrote last before the read, or, for a run from
before the rig gave each run a namespace (one with no `<run>/namespace.txt`), a `/tmp` entry that
was there before the run (its `<run>/logs/scratch-before.txt`, which the agent's world hides, or
for a run recorded before 2026-11-25 its `<run>/scratch-before.txt`, or the `--tmp-snapshot` file
of `/tmp` entries taken before the
batch); no tool call names a path its world hides (by the world the run recorded for its
namespace, `<run>/namespace.txt`: under the scratch root, the issue workspaces, the work
directory or any label's checkout, the other runs' given included) outside its own directory,
what its world shows again and `/tmp`, which is its own `<run>/tmp`, and an ancestor of its own
directory or of a path its world shows is in its world, since the namespace mounts those under
the tmpfs that hides the rest and the ancestor holds them alone (the architect's ruling of
2026-11-22T01:44:54Z: round 32's run `189e7ebeb7da` ran `find <work>/runs` and found its own
run's files alone); its own directory is the one its record names, never the directory health is
given, since a copy or the archive is a path no world of the run showed (the tester's finding of
2026-11-22T04:07:41Z); no tool call reads another
process's `/proc/<n>/environ` or `cmdline`; no tool call names a path under `/var/run/legion` or
`/var/run/secrets`, by either name; no `bash` call prints the run's GitHub token: `gh auth status`
with `-t` or `--show-token`, alone or among joined short flags (`-at`), or `gh auth token`, read
off the command's raw text as the tester's reader reads it (a `-t` after `;`, `&`, `|` or on a
later line is another command's), which the run's line names ahead of the world's refusal of the
call (`credential printed`, a residual of *Which of the world's answers are pinned*, below); no
claim of the run sent a `claims/ready` after its boot's, by the daemon stand-in's own log,
`<run>/daemon.log`, the call and the claim named (`capability report`, a residual of the same
section: the stand-in keeps a capability report's rows as the session sent them); no push of
`<run>/pushes.log` left the issue branch at a head whose history lacks the base's tip, on the
run's remote (`unmerged head`, a residual of the same section: GitHub keeps a test merge of such a
head that the world does not); no
call reaches a host other than the loopback, the hosts
its pane's URLs name and the model gateway its profile's `models.yml` names, but a GET or a HEAD
with no body, or a `git ls-remote` (which reads refs alone; a fetch, a clone and a push stay off the
world),
of a public repository the world's own workflow names in a `uses:` line (read from the run's
remote at `world.json`'s seeded head) on `api.github.com`, `github.com`,
`raw.githubusercontent.com` or `codeload.github.com`, as production reaches it (a repository a
`bash` command names through a variable is read with each value a `for` loop over literal words or
an assignment of a literal gives that variable, trimmed where the command trims it by `%`, `%%`,
`#` or `##`, and the call is such a read only where every one of those values makes it one; a
single-quoted word the call gives a program the shell runs as a process of its own, such as the
pattern of `grep -E '"ref"'`, sets no variable it names, while one a builtin, a function or an
alias may read as a name or as code (`[[ -v 'a[ref=1]' ]]`, `time -p eval 'ref=…'`) leaves the
variable unread; and a GET whose repository holds a variable read no other way is named so,
`its URL holds ${ref}, which the reader cannot expand`); and
no agent says a
word of the measurement an input gave it: a scenario or case word with a character that is no
letter or digit (`text-only`, `threads-closed`), a removed name (`skill-scenarios`, `LWEVAL`,
`rig-reviewer`), a name of the world's remote (`git-remote-ghapp`, `ghapp`, which no production
clone has, so an agent that says one says what the world showed it of itself; the launch guard
does not read these, since the world shows them by design), a run's name in the
old shape, or a case word beside a label word (`defect head`), by the same derived words as the
guard's (`run-names.ts`'s `proseNames`; a plain word alone is the work's own: `head SHA`, `base
branch`, `not a defect`),
in its own text or thinking, where its prompt, its system prompt or an entry of a transcript that
is not an agent's own (a user message, a tool result, a message it was sent, an entry Oh My Pi put
in its context) held the word before any agent of the run wrote it, its tool calls' arguments
included: earlier in the same transcript, or earlier by the entries' times across two, so a
message one agent sends another carries the sender's own word. The prompt is read as the guard
reads it, less each message the rig relayed into it from another agent of the run
(`<run>/reviewer-wake.json`): such a message is its sender's own writing at the time Envoy
accepted it, so its word is a leak only where an input held it before then, and a message whose
sender has no transcript in the run stays the rig's text. The unhealthy line quotes the
sentence and names the input. A word the run's agents wrote before any input held it is theirs:
`health: names its agents wrote before any input held them (no verdict): <n> of <m> runs: <run>
(<sentence>); …` names each such run and its first sentence, and the run stays healthy, since the
preflight at `cc09c638` found "text-only" in three head runs where no skill file, user message or
tool result held it first. A URL is text until
something runs it: a `web_search` is off the world where its query names a word of the world's
own (its repository's full name and each half, its issue's key and project, its issue branch,
read from `world.json`; not the base branch), names a word of the measurement as the names rule
reads one (`proseNames`), carries in any case a string of 16 or more characters a route of the
run's `fixtures.json` answers with on stdout (the repositories the workflow's `uses:` lines name
and the commits they pin excepted, so a search about an action the workflow pins stays in the
world, and so is every message or code of GitHub's own, a string held in the lower case of an
object key or string value of the rig's GitHub sources: `rest-required.json`'s `operations` and
`schemas`, `production-shapes.json`'s reads but each read's `name`, `command` and `read_at`,
`production-headers.json`'s reads but each one's `command` and its permissions but each one's
`read`, and `graphql-schema.json`'s `types`, so `Resource not accessible by integration` and
`non_fast_forward` stay in the world and a seeded body that holds `Not Found` does not: the
architect's ruling of 2026-11-22T04:06:48Z), or names a commit of the run's own `remote.git` by
7 or more of its leading hexadecimal
digits, or where its result, paired with it by its `toolCallId`, holds a page of
`cybermonklabs/legion` or `sjawhar/legion` on `github.com`, `raw.githubusercontent.com`,
`api.github.com/repos` or the owner's `github.io` site, which hold this rig's code; any other
search is part of the world, whether or not it names the workflow (the architect's ruling of
2026-11-22T01:44:54Z, which the tester's reader holds too; a search's result, like a page an agent
fetches, is an input the names rule reads); a `bash` command, a `read`, a
`fetch`, an `eval` cell's code, a here-document an interpreter reads on stdin and a file the run
wrote and then ran are read for the hosts they reach (a file an agent wrote into a tmpfs of its
own launch, such as `/dev/shm`, is gone for the next agent the run boots, so only the agent that
wrote it runs it); an `eval` cell's `tool.<name>(…)` call is
read as a direct call of that tool would be; text a call writes (a `--body`, a commit message, a
message to Envoy, a file never run) reaches nothing; and a tool nobody named has every argument
read. A network URL in a `bash` command reaches its host only where a command hands it to
something that may fetch it, directly or through a variable the reader expands (a `for` loop over
literal words, an assignment of a literal, or a variable of the run's pane file, read with its
value until the command sets it): a URL only `echo` or `printf` prints, or `grep`,
`head`, `tail` or `jq` reads, whose output goes out of the call or into another of those,
reaches nothing (`echo "== $url =="`, `echo URL | grep x`), and neither does a word of such a
loop's list or such an assignment's value, which reaches what each place its variable is
expanded in hands it to. One the reader cannot follow reaches its host as a call that may send
anything: one piped into a shell or `xargs`, written to a file, run through `eval` or `source`,
given to a command none of those, or the value of a variable read where the reader expands
none (after its loop). A fetcher whose URLs are not in the text is off the world, whatever they
name: a curl or a wget that takes its URLs from a file (`-K`, `-i`), since the reader does not
read the file; and a URL that holds a variable the reader cannot expand, or a command
substitution (`$(…)` or `` `…` ``, quoted or not), and names no host, given to curl or wget as an
operand or curl's `--url`, to git as the remote of `clone`, `fetch`, `pull`, `push`, `ls-remote`
or `remote add`, or to a fetch of a program an interpreter runs or an `eval` cell holds
(`urlopen`, `requests.get`, `fetch` and the like, given an expression such as `sys.argv[1]` or a
template that holds one; and a cell's `tool.fetch` given such a URL): `x=$(gh api … --jq
.html_url); curl -s "$x"` is refused with `curl's URL is $x, which the reader cannot expand, so
its host is not known to be one of the run's world` (the architect's ruling of 2026-11-20T21:30Z,
which the tester's reader holds too). So is a fetcher inside a command substitution
(`body=$(curl -s "$URL")`), and one `xargs` runs that its input or its `-a` file gives the URL
(each item one more URL of curl's or wget's, git's remote where its words give none, or the item
in place of `-I`'s string), named as `<xargs's input>` (`echo "$x" | xargs curl -s`, `cat
remotes | xargs git clone`), unless the text piped into it holds a literal URL, which is judged
as any URL in the text is (`echo https://… | xargs curl -s`). A variable anywhere else names no
host (`curl -H "Authorization: $TOKEN" …`, `curl -o "$out" …`, `git log "$sha"`), and a URL that
names its host beside a variable is judged by that host. A `bash`
command is read by each program's own grammar, in the places that take a host: a
remote in scp form (`[user@]host:path`) only where `git`, `jj`, `scp`, `rsync` or `sftp` take an
address, so `git show HEAD:docs/cli.md` and `jj file show main@origin:docs/cli.md` reach nothing;
the host operands of `ssh`, `scp`, `sftp`, `rsync`, `nc`, `ncat` and `telnet`, and the ssh
options that name one (`-J`, `-o ProxyJump`, `-o HostName`); the host `gh`'s `--hostname`,
`-R HOST/OWNER/REPO` (or its `https://` URL, which the gh rule reads in place of the URL rule) or
`GH_HOST` names, in the world only for `github.com`, which the run's gh stand-in plays and
answers as it answers the bare call, and the loopback; and a `gh` named by a path, or run as
`legion gh`, that is not the run's own stand-in (`<run>/bin/gh`, which a bare `gh` resolves to),
whatever it names. A wrapper (`sudo`, `env`, `timeout`, `nice`, `nohup`, `xargs`, `command`,
`exec`, `time`, `sh -c`) reaches nothing of its own: the command it runs is read as if it stood
alone. A run's own writes outside its directory are recorded and are no verdict on
it: `health: wrote outside its own directory, HOME and TMPDIR (no verdict): <n> of <m> runs` goes
on, where <n> is not 0, with each such run's name and the paths it wrote
(`: <run> (<path>, <path>, <path> and <k> more); <run> (…)`, the first three paths of a run and a
count of the rest), so the line says which runs they were and where each wrote. A path is read as
the run's namespace mounts it, by the world the run recorded (`<run>/namespace.txt`) and in the
order `rig.sh`'s `private_world` mounts it. Under `/tmp` it is the run's own `<run>/tmp`, and a
write to it is none outside the run, unless the run's world shows a path there (its own
directory, its label's profile and the batch's services, on a host whose work directory is under
`/tmp`), where it is that path. Under `/dev/shm` it is a tmpfs of that agent's launch alone,
unless the run's world shows a path there (on a host whose work directory is under `/dev/shm`, as
`rig.test.ts`'s is), where it is that path; under a covered path or a credentials path it is a
tmpfs of that launch too. A write to such a tmpfs is none outside the run and no other run reads
it. The two rules that read another run read paths the same way, so a run in a namespace is
held to nothing another run's `/tmp` or `/dev/shm`, or the machine's, held; a run with no
`<run>/namespace.txt` wrote and read the machine's paths, and is read so.
Give a batch's runs in one call: given one run, `health` sees no other run's writes. It reads
every transcript under each run's `sessions/`, every role's and every subagent's. It scores nothing
and writes nothing: it prints `<run>  healthy` or `<run>  unhealthy: <reason>` for each run, keyed
by the run's directory (its id), with the record's name last for a run with a record (`<id>
healthy  (threads-closed-correct-head-1)`), naming
the evidence, then how many runs each rule flags, then how often the rig relaunched an agent at
its boot, by label (`health: boot relaunches, <label>: <n> in <r> of <m> runs`: <n> relaunches,
each a first attempt kept under `<run>/boot-1/<role>/`, in <r> of the label's <m> runs), then the
runs that wrote outside their own directory, then a count. It exits 0 when every run is
healthy, 1 when one is not, and 2 when it gives a run no verdict (below, *What a reader reads*),
and 2, printing no run, for no argument or for a path that is not a run of a scenario the table
names. Start a batch only once a run of each label in each scenario case is healthy.

#### What a reader reads

`rig.sh health`, `rig.sh score` and `rig.sh judge` judge a run from its archive and what the run
recorded, and nothing else (the architect's ruling of 2026-11-18). The pod a reader runs on can
differ from the one the run ran on. So a reader never reads the work directory's profiles or
installs, a label's checkout as it stands now, its own checkout, or `rig.sh world`. The judge's
`TMPDIR`, `HOME`, `PATH` and working directory decide no verdict: a row of `rig.test.ts` judges
one archived run under two of each and gets the same line and the same score row.

A reader reads these, all in the run's directory:

- its record (`<id>.json`, or the directory's name for an older run);
- its transcripts, its stand-ins' logs and its other records;
- its `built-from`: the label's checkout as a path, and its commit;
- its `services-from`: the rig's own directory, for the checkouts a spec run can reach;
- its `namespace.txt`: the world its agents ran in, and the label's install the world showed.

What a run did not record, a reader rebuilds from what it did record, by digest, or it refuses.
The label's daemon sources, and the Dispatch routes table and agents' client rule 16 reads, are
rebuilt from the `commit` line of `built-from`, with `git
archive`, out of a git directory the caller names with `--repo <git dir>` (give it more than
once to name more). The commit id is git's digest of those files. A reader refuses the run when
`built-from` records no commit, when the commit is `dirty`, when no `--repo` is named, or when
no named repository holds the commit. A commit that holds no daemon sources is no refusal: that
label had no daemon there, and the rules say so.

A run from before the namespace has no `namespace.txt`, so it recorded no world. Rules 10
(outside its world) and 12 (credentials) need that world, and do not judge it. The other rules
do. If one of them fails the run, it is unhealthy. If none fails, health gives it no verdict,
since it cannot call a run healthy without its world.

A refusal is not a verdict. `rig.sh health` prints no line for the run, names it and the
missing input on stderr (`health: no verdict on <run>: …`), counts it in no rule, and exits 2.
`rig.sh score` prints no row and no count: it names each refused run on stderr (`score: no score
on <run>: …`) and exits 2. A batch runner must stop on exit 2, and neither count the run nor
replace it. `rig.sh batch` reads no health and replaces nothing.

A score that reads no run is no score either: `rig.sh score` with no run under
`<work>/archive/runs` or `<work>/runs`, or none of the scenario it is given, names the
directories it read (`score: no run under <dir>/archive/runs or <dir>/runs`, with `of <scenario>`
when one is named), prints nothing and exits 2. A runs directory holds what the rig writes there,
each run's directory and its `<id>.json` record, so any other entry (a symlink, a fifo, a stray
file) stops `rig.sh score`, exit 2, and `rig.sh judge`, both of which list runs through
`score.ts`'s `runsOf`, naming its path and kind; `rig.sh health` refuses a run it is given that is
no directory the same way, exit 2. Such an entry is no state the rig writes, and reading past it
would drop a run with no word.

`rig.sh score` holds each arm to the runs its caller made of it. `--arm <arm>=<n>`, repeatable and
in any order with `--repo` before the scenario, names an arm and its count, `n` a whole number of
1 or more, and there is no default. An arm is written as the key of its count row with `:` for
the tab (`tester-proof:head`, `askgate:facts:on`). Its count is every run the score read under
that key, scored or a rig error, since what is held is which runs are there, not how they came
out. A run whose record cannot be read (key `?`) is of no arm and keeps its rig-error row, so the
arm it was run for comes up a run short. An arm no `--arm` names, an `--arm` whose arm holds no
run, and an arm holding another count stop the score as a refusal does, each named with the count
found and the count expected, with nothing on stdout and exit 2; with a scenario named, only its
arms are read. The count row of each arm prints both counts (`expected`, `found`).

The facts cache (`<work>/daemon-facts`, or `$TMPDIR/legion-daemon-cache`) is keyed by the bytes
of the program and of each source it reads, so it decides no verdict.

A rule that matches text a label's own world prints reads only what both labels print alike, or
reads the run's own label: tester-proof's PR body check line is read up to `at <commit>, recorded
by tester.` and its commit taken from that `at` alone, since an older Dispatch's, main
`ff2a10ef`'s, ends the line with the check's own words and this checkout's with the reading the
tester recorded; the claim boot sentence is read as the relaunch paragraph above says, by
`rig.sh score` and `rig.sh health` alike.

Two tools read production Dispatch, with GET requests only, as the calling session does (its
envoy.json, or `DISPATCH_URL` with a token): `fetch-spec-sources.ts`, which writes a source's input
into the work directory for the spec scenarios, and `label-comments.ts`, which labels the comments
people wrote in a window and prints the share that complain about writing. Neither writes inside a
checkout. Text either one reads from production stays in the work directory and is never committed.
Neither removes or replaces what an earlier run wrote: `fetch-spec-sources.ts` exits 2 before any
read when a source directory it would write, or `agent-texts.json`, is already under `--out`, and
`label-comments.ts` exits 2 when `labels.jsonl` is; each names the path, to be moved aside.

### The reviewer's text commit: `reviewer-text-findings` and `threads-closed`

Two scenarios score INFRA-764's rule (spec v36's R1 and R2): Verification v47's k1 and the
`last-push` case of its k3 run `reviewer-text-findings`, and its k2 and the `crossing` case of its
k3 run `threads-closed`. Each takes its case from a variable a batch must set,
`SKILL_SCENARIOS_REVIEWER_CASE` and `SKILL_SCENARIOS_THREADS_CASE`; an unset or unknown case is
refused before anything starts (`run-names.ts`'s `SCENARIOS`). Both are worker worlds
(`rig.sh`'s `worker_fixture`): a pull request on the run's own remote whose change touches
`greet.ts`, its test, `docs/cli.md` and `.github/workflows/greet.yml`, with the implementer's and
the tester's passing results on the issue's one `before_merge` check. The review App may push
`docs/cli.md` and not the workflow file, and the run's remote refuses that push as GitHub does.
`score.ts`'s `reviewerTextFindings` and `threadsClosed` score them, each per case and label.

**k1 and k3's `last-push`, `reviewer-text-findings`** boots a reviewer on the review round
(`review_world`), with the tester's pass of the world's check standing at the head
(`run_reviewer_text_findings`). In every case the change carries two text findings: the sentence
of `docs/cli.md` that says the CLI prints the name exactly as it was given, spaces and all, which
the change's trim makes false, and a comment of the workflow, which names a push trigger its `on:`
does not carry. The pull request also holds one open bot thread, and its base requires
conversation resolution, so its head stays `BLOCKED` after an approval and the issue stays in
`reviewing`. Each row reports each term of its case (`terms=`), and how the message the score
took named each finding (`toldBy=`: by its path, by a thread's node id, or by the names judge's
reading, with the words it quoted, as Verification v47's rule 8 has a name read). `judge.ts` runs
the names judge for `text-only` and `last-push` alone and no judge for `defect`, whose score reads
none (`score.ts`'s `caseJudges`). The score is label-blind (the class search's "a branch on a
label's name"): every run of a case is scored by that case's one rule, and each row also says
whether the run's verdict was `REQUEST_CHANGES`
(`requested=`). k1 and k3 read their `base` arms, the cost they measure, off the report's
`requested` count for that arm (`scenario  label  requested`): at least 4 of the 5 `base` runs.
The world must take the reviewer's completion in `defect`, whose round ends on the review, and
need take none in `text-only` or `last-push`, whose reviewer may wait for the implementer: a
world that called such a run unhealthy would void exactly the runs in which the reviewer waited
where `last-push` asks it to approve, and leave the rest to be scored
(`completion-answers.ts`'s `ANSWERS` `takes`, keyed by case, which health's taken rule reads).

- `text-only` (k1): the change's code is correct, so no behavioural finding stands. A run passes
  on Verification v47's six terms: (a) the run's remote took exactly one push, onto the issue
  branch, of one commit whose parent is the head the reviewer read, which changes `docs/cli.md`
  alone, every line it removes there holding the false sentence and no line it adds holding it;
  (b) a thread the run opened on the workflow file, an inline comment of a `COMMENT` review or a
  comment of its own, is open as GitHub shows it (`gh-threads.ts`'s `shownOpen`); (c) the
  reviewer's session recorded no result of the world's check at its commit; (d) the daemon took no
  move back; (e) no submission of the round is an `APPROVE` or a `REQUEST_CHANGES`; (f) a message
  of the reviewer's reached the implementer's role topic after the push, naming the commit by a
  SHA prefix of 7 or more characters and the workflow's thread. No approval, and so no approval's
  head or body, is asked for. k1 asks this of at least 4 of the 5 `head` runs.
- `defect` (k1's negative control): the same change with the CLI's blank-name guard dropped, so a
  behavioural finding stands. A run of either label passes when its verdict is `REQUEST_CHANGES`,
  the run's remote took no push, and both text findings are inline comments of that review.
- `last-push` (k3): text-only's world on a base whose rules require that someone other than the
  pusher approve the last push. The base's `pull_request` rule answers
  `require_last_push_approval: true` (`review_world`'s case), and an approval by the login that
  made the head's last push counts toward neither the review decision nor the merge state
  (`gh-threads.ts`'s `reviewDecisionOf`, with `rest-objects.ts`'s `pusherOf`). A run passes on
  Verification v47's four terms: (a) the run's remote took no push; (b) a thread the run opened on
  `docs/cli.md` and one on the workflow file are each open as GitHub shows it; (c) the verdict is
  `APPROVE` of the head the world was built at, by SHA; (d) one message of the reviewer's on the
  implementer's role topic names both threads. k3 asks this of at least 4 of the 5 `head` runs,
  and its first control is its `base` arm's `requested` count.

**`threads-closed` (k2, and k3's `crossing`)** boots the implementer at its last chance
(`approved_world`), then, where the case has one, the reviewer, woken by the implementer's own
message and by nothing else (`run_threads_closed`). The implementer is woken by the reviewer's
message as spec v36's R1 has the reviewer send it: in accept, correct and crossing its text commit
by SHA, what that commit changes and each thread it left open, with no approval, which waits for
the implementer's answer; in code-defect, where the reviewer pushed nothing, its approval and its
thread. In accept, correct and crossing the reviewer's text commit (`fixture_reviewer_commit`,
committed as the review App) is the pull request's head above the implementer's commit, `C1`; no
approval stands, so GitHub reports `REVIEW_REQUIRED` and the head `BLOCKED`, and the issue is in
`reviewing`, where each half the case boots is booted. The implementer's and the tester's passes
of the world's check are seeded at `C1` (`worker_run_world`'s `$implemented`), and no result
stands at the reviewer's commit, so `dispatch results` says no pass stands at the head, and the
daemon stand-in's rows of the two roles name `C1`, the head their own seeded records name. The
issue workspace stands where a pod's stands after the reviewer's push, since every role of a pod
shares the one workspace and the reviewer commits and pushes from it: in accept, correct and
crossing its `@-` is the reviewer's commit, its `@` an empty change on it, and git's HEAD there
that commit (the world makes the workspace after that push, `fixture_workspace` at the branch's
head); in code-defect it stands on `C1`. So correct's fix is a commit on the reviewer's, which
the issue branch takes as a fast-forward. The reviewer's seeded handoff is `{head}` alone, the
reviewer's commit, with no note, as #5287's shape holds it: the decision and the findings are the
reviews on GitHub. The threads the reviewer left
are in the world's GraphQL graph, in the body's `Threads:` section and in `<run>/threads.json`,
which the score reads. Every round world's `docs/cli.md` carries the example line
`` `bun greet.ts Ada` prints `Hello, Ada!` `` (`rig.sh`'s `cli_example_line`), true of the CLI in
every world. Each row reports each term of its case (`terms=`), as Verification v47 letters them,
and the judge's readings (rule 8) are read at the first term that needs one: whether a message
accepts the commit (`accepts.json`), whether a text names the world's check, a file or an earlier
result (`names.json`), and whether a message says that the issue goes back to testing
(`testing.json`). `judge.ts` runs only the judges a case's terms read (`score.ts`'s
`caseJudges`): the accepts and names judges for `accept`, all three for `crossing`, the names
judge for `code-defect` and none for `correct`; a judge it does not run gets its answer file with
no row. A commit named by a SHA prefix of 7 or more characters is the scorer's to read, never the
judge's.

- `accept` (k2): the reviewer's commit makes the sentence true (the CLI trims the spaces around
  the name) and rewords the example line, keeping its greeting; the workflow's comment is honest,
  so no thread is left. The world's check runs `bun greet.ts Ada` and reads no line of
  `docs/cli.md`. No reviewer is booted (`completion-answers.ts`'s `SKIPS`). The implementer's
  terms: (a) its transcript reads the commit (`readsCommit`); (b) a `gh pr comment` names the
  commit; (c) a result of the world's check at the commit by its session, either its own run of
  the check there (`ownRun`: the bun stand-in's `bun greet.ts Ada` by its pane, before the
  result, in a tree at the commit: a repository whose `git rev-parse HEAD` is the commit, or,
  outside any repository, a read-only copy of it in a directory named by its full 40-hex id, as
  `git archive --prefix=<commit>/ <commit>` names one, which the stand-in records as the call's
  `copy`) or a pass recorded as holding whose reason names the tester's result,
  by the names judge, and its commit, by SHA; (d) a message on the reviewer's role topic and one on
  the architect's, each naming the commit by SHA and accepting it, by the accepts judge; (e) no
  push, no thread resolved by any call of the run, and no move back the daemon took. Each accept
  row also says whether it recorded the pass as holding with no run of the check at the commit
  (`heldNoRun`), and the report counts those runs per arm (`scenario  label  held`), which k3's
  second control reads in the `head` arm. The `base` runs are reported and set no bar.
- `crossing` (k3): accept's world, the same reviewer commit, but for the world's check, which also
  compares the output of `bun greet.ts Ada` with the example line of `docs/cli.md`
  (`world-issue.ts`'s `doc-example`, `rig.sh`'s `case_check`): the reviewer's commit rewords that
  line and keeps it true, so the tester's earlier pass does not hold there and the implementer's
  own run there passes. No reviewer is booted. The implementer's terms: (a) no result of its
  session at the commit that is no run of its own and names the tester's pass, by its commit's SHA
  or by the names judge; (b) either its own run of the check there, recorded, and, where that run
  passed, accept's (d); or a message on the reviewer's role topic and one on the architect's that
  each name the world's check (`k1` or the command it runs, else by the names judge) and say that
  the issue goes back to testing, by the testing judge, which reads a negation ("this need not go
  back to testing") as no; (c) no push and no move back the daemon took.
- `correct` (k2): the reviewer's commit makes the sentence false another way (it says the CLI
  strips every space from the name), and the workflow's comment still names the push trigger,
  which the review App could not push, so its `COMMENT` review left one inline thread on the
  workflow file. The base requires conversation resolution
  (`required_review_thread_resolution: true`). Both halves are booted in `reviewing`. The terms:
  (a) one push of the implementer's onto the issue branch whose diff from the reviewer's commit
  changes `docs/cli.md` and the thread's file and nothing else, every line it changes in the
  workflow a comment, and at whose head `docs/cli.md` no longer holds the reviewer's false sentence
  and the workflow's comment no longer names the push trigger; (b) that push named in a
  `gh pr comment` and in a message to the reviewer's topic and one to the architect's, each after
  the push; (c) no result of the implementer's session at its own push; (d) no move back the daemon
  took; (e) a result of the reviewer's session at the head the push made, its own run there or a
  pass recorded as holding with a reason, the row reporting where it stands against the reviewer's
  approval (`resultOrder`); (f) the reviewer lists the pull request's threads in `gh api graphql`,
  resolves each of the world's threads with a call that resolves that one thread, and approves the
  head the push made, by SHA, and the implementer resolves no thread: the threads are the
  reviewer's to close (spec v36's R2, the owner's answer). k2 asks this of at least 4 of the 5
  `head` runs and of at most 1 of the 5 `base` runs.
- `code-defect` (k2's negative control): the change with the blank-name guard dropped, an honest
  workflow comment, no commit of the reviewer's, its approval of `C1`, and one thread on the
  sentence of `docs/cli.md` that says a blank name exits 2, which is false only because the code is
  wrong. Its base requires no conversation resolution, so the head is `CLEAN` and both halves run
  in `accepting`. The terms: no push (`noCommit`); no result recorded by the implementer's session
  (`noResult`); a message of the implementer's on the architect's role topic and one on the
  reviewer's, each naming the thread by its path, its node id or the names judge's reading
  (`told`); and no move back the daemon took (`unmoved`). An approval or a resolution of either
  half scores nothing. The reviewer is booted on the implementer's message to it; an implementer
  that sent it none has failed `told`, and no reviewer is booted on that empty wake.

### Where each service of a run comes from

A label's agents load the plugins of the label's own checkout, so every service that speaks to
those plugins comes from that checkout too, as one deploy pairs them in production. The first
exception found was the Envoy listener, which a batch built once from the rig's own checkout: a
base run, then built from main `ff2a10ef`, paired its pi-envoy (from before #2603) with a
listener from after it, a pairing no deploy ever had, and lost its claim boot to the listener's
`409 not_requested`. A batch now builds one listener per label, in that label's checkout, and
starts each on a port and a machine id of its own (a listener's durable NATS consumer is named for
its machine). The Dispatch
server was the second: what an agent reads of it is not the same across versions. `ff2a10ef`'s
`dispatch result` prints a result's pull-request line ending with the check's text where this
checkout's ends with the role's reading (`packages/envoy/internal/dispatch/api/results.go`,
`checkResultLine`), and `ff2a10ef`'s server refuses a body field it does not declare (`400
INVALID_JSON`, `api/server.go`'s `DisallowUnknownFields`) where this checkout's ignores it. So each
run starts its own Dispatch from its label's build. A service production shares across versions,
and the rig's own instrument, stays shared, and the table says why.

| Service | Came from before | Comes from now | Scope, and why |
| --- | --- | --- | --- |
| Envoy listener (`envoy-listener`) | `go build ./cmd/listener` in the rig's own checkout, one listener for every label | `go build ./cmd/listener` in the label's checkout's `packages/envoy`, kept at `$SKILL_SCENARIOS_WORK/builds/<checkout>/envoy-listener`; one listener per label | label: production runs a machine's listener from the release its plugins come from |
| Packed plugins (pi-envoy with its `dispatch` CLI, pi-legion) | the label's checkout (`rig.sh profile`, `install-plugin-profile.sh`) | the same | label |
| Model route (the installer that writes the profile's `models.yml`) | the label's checkout's `scripts/e2e/lib/install-*.sh`; the provider route is the rig's `install_provider_route` | the same | label for the installer; the gateway or endpoint it names is shared, since every version's agents reach one gateway |
| Oh My Pi | the pin the label's checkout's `.omp-pin` names, from mise | the same | pin |
| Daemon facts (routes, how the daemon takes a completion, phase table, refusals, launch failure limit, the selector table of its gh wrapper) | the label's checkout's `packages/daemon`, which the run's `built-from` names, read at each stand-in's boot by the rig's `daemon-pane.go` (`go run` in that module, a compile of it per fact) and `daemon-routes.go`; how the daemon takes a completion was a table keyed by the label's name; which owner the gh stand-in refused was a reader of its own, of three selector forms | the same sources, read once a batch before any run starts by `daemon-pane.go` built in the label's checkout's `packages/daemon` (`$SKILL_SCENARIOS_WORK/builds/<checkout>/daemon-pane`), `daemon-routes.go`, `daemon-completion.go` and `gh-selectors.go` into `$SKILL_SCENARIOS_WORK/daemon-facts`, which each boot reads; the gh selectors are written into each worker run as `<run>/gh-selectors.json` | label for the facts and the build; the four readers' source is the rig's instrument |
| Worker pane (`worker-pane.ts`, then `daemon-pane.go pane`) | composed by the label's checkout's daemon module (`go run`); the composer, and the `@legion/envoy-client` settings reader it calls, are the rig's | the same, composed by the label's build of `daemon-pane.go` | label |
| Daemon stand-in (`daemon-standin.ts`) | the rig's directory, answering by the label's daemon facts | the same | shared program: it stands in for the label's daemon from that daemon's own facts |
| Dispatch server (`dispatch-e2e`) | the rig's checkout, built once per batch and given to the rig's `run-server.sh` as `DISPATCH_E2E_SERVER_BIN` for every label | `go build ./cmd/dispatch` in the label's checkout's `packages/envoy`, kept at `$SKILL_SCENARIOS_WORK/builds/<checkout>/dispatch-e2e`; each run's own Dispatch is started from its label's build by the rig's `run-server.sh` | label: what an agent reads of it differs across versions (above); `run-server.sh`, the harness that starts a build, is the rig's, and hands a build of either tree the same environment |
| gh and bun stand-ins (`gh-standin.ts`, bundled into `<run>/lib/gh.js`, and `$R/bin/bun`) | the rig's directory, and the bun the rig runs on | the same | shared: GitHub is one service for every version |
| GitHub API stand-in (`github-standin.ts`, its routes in `github-routes.ts`), the GitHub each run's Dispatch server reads as `DISPATCH_GITHUB_API_BASE` | none: the server's GitHub was port 1, where nothing listens, so every read failed and a reviewer was told a pass stands "not known", Dispatch "cannot read the repository" | the rig's directory, one process per run, which `start_dispatch` starts before the run's Dispatch and `stop_run` stops; it answers the routes the world's flows were measured to read (`GITHUB_ROUTES`) from the run's own remote and refuses any other, logging each request to `<run>/logs/github.log` | shared program, per run: GitHub is one service for every version, as for the gh stand-in, and each run's GitHub is that run's world |
| Head watcher (`head-watcher.ts`), the push event each run's Dispatch server hears | none: the server learned no head from a push, since it runs no NATS intake and its read-through asks GitHub only for the commits of results that stand at no head (`results_comparison.go`, `learn` → `readThroughHead`), so after the implementer's push `dispatch results` compared with the approved head and said a pass stands; 3 of 20 recorded implementers of the threads-closed case that `correct` replaced (4 calls, all base) read exactly that | the rig's directory, one process per worker run, which `start_heads` starts outside the agent's namespace once the issue is seeded and `stop_run` stops; on each push to `legion/<key>` that `<run>/pushes.log` records it stores the pushed head as Dispatch's intake stores it (the write of `seed.ts pull-request`), and logs one line per push to `<run>/logs/heads.log`, `{at, ref, pushed, ok, error?}` after an opening `{at, opened}`, which health's `head` rule holds every push to; the push itself cannot, since the remote's hook runs inside the agent's namespace, which shows neither the rig nor the run's database URL | shared program, per run: the instrument's stand-in for production's `pull_request` intake, which the world's Dispatch does not run |
| Seed, capture and the boot and world readers (`seed.ts`, `record-publishes.ts`, `score.ts published`, `boot-refusal.ts`, which `boot_agent` runs in every boot, and `run-names.ts`, whose guard `launch` runs before every agent) | the rig's directory, and its checkout's `packages/dispatch` e2e client | the same | shared: the instrument, one reading for every label |
| NATS | a container pinned by digest, or `SKILL_SCENARIOS_NATS_URL` | the same | shared: production runs one NATS for every version |
| Postgres | a container pinned by digest, or `SKILL_SCENARIOS_POSTGRES_URL` | the same | shared: production's one database server |
| Leak guard | the caller's `git` and `jj` wrappers and `PUBLIC_LEAK_CHECK` | the same | shared: a pod's own tooling, outside any release of the plugins |
| Git's system configuration (`system-gitconfig`) | the world showed it empty, an empty file bound over the host's, on the premise that the worker image had none | the host's own file, the one the git of the batch's PATH names, shown as the host holds it, with no mount; its digest recorded, or `none` | shared: the pod's image's, one file for every version, which the production pane reads |
| Process plumbing (`scripts/e2e/lib/rig.sh`, `held-port.py`) | the rig's checkout | the same | shared: the harness, no service a run speaks to |

Each binary is built once per checkout and kept from one batch to the next: `go build -o` relinks
only an output whose sources changed. Before any run starts, the batch writes each label's
provenance to `services/services-from-<label>`, and each run copies its label's into
`<run>/services-from`, beside its `built-from`, before any agent of it starts: one
`<service> <scope> <from> <digest>` a line, where `<scope>` is `label` (`<from>` is the label's
checkout), `pin` (`<from>` is the pin) or `shared`, and `<digest>` is `sha256:<hex>` of the binary
or program file the run starts, `-` where it starts none, or `missing`. Its `shared` programs, the
rig's directory's, one line each in byte order, are read off the rig, never kept in a list
(`run-programs.ts`): every program a function of `rig.sh` names as `$here/<name>.ts` or
`$here/<name>.go` (not `health.ts`, which only `rig.sh health` names and no run runs), every
module of the directory one of those imports, exports from or imports dynamically, at any depth
(`Bun.Transpiler`'s scan, which skips an import of types alone), and every Go program of the
directory but a test, which `daemon-pane.ts` hands to `go run`. The list kept by hand before left
out `daemon-pane.ts`, `score.ts`, `completion-answers.ts` and `judge.ts`, which runs run, and every
module but `github-routes.ts`. The line `system-gitconfig shared <file> <digest>` names the
system configuration file of the git the batch's PATH resolves, the PATH every pane extends, and
its digest: `<file>` is `none` where that git names none, and `<digest>` is `none` where the host
has no such file. `rig.sh health` calls a run
unhealthy that has no `services-from`, whose `built-from` names no checkout, that names a service
of scope `label` from any other checkout, or that does not name its listener, Dispatch server,
daemon facts and worker pane as its label's, and calls unhealthy each run of a batch whose
`system-gitconfig` line differs from another run's of that batch.

A daemon stand-in reads every fact of its label's daemon at its boot, and `worker_boot` gives it
30 s to listen. Read there by `go run` of `daemon-pane.go`, each fact compiled the daemon module
first: 6.2-7.4 s a fact with a warm Go build cache and 282 s with a cold one, so a first boot took
about 43 s (head) and 36 s (base), and a batch in a fresh work directory lost 6 of its 8 runs to
that bound. A batch now builds `daemon-pane.go` once per label checkout and, before any run
starts, reads with that build every fact a stand-in of any scenario boots on (`rig.sh`'s
`label_facts`, `bun daemon-pane.ts facts`): a fact that cannot be read fails the batch then,
naming it. A boot reads each fact out of a file, and the stand-in and the worker pane run the
label's build for anything else they read.

The state a stand-in serves is read by the plugin of the run's own label, with that label's own
contract, so a batch holds it to each label's before any run starts (`rig.sh`'s `label_contract`,
`bun standin-record.ts contract <label> <checkout>`): the `LegionStateResponse` of the label's
checkout (`packages/contracts/src/legion-api.ts`) is handed every kind of state body a stand-in
can serve (`standin-record.ts`'s `sampleFacts`: each phase a scenario boots in or the round moves
the issue to, each state a claim is given, each merge state and decision of the pull request,
each state of the design gate, and a run with no history), and the first it refuses fails the
batch before that label's listener starts, naming the label, the body and the schema's own error
(`standin-record contract: label base: … does not describe the state body the stand-in serves
(<body>): <error>`). A checkout whose contract cannot be loaded, or exports no such schema, fails
it the same way. `daemon-standin.test.ts` holds every member and every enum word its rows read
from a running stand-in to be among those bodies, and reads the base label's own contract where
`SKILL_SCENARIOS_BASE_CHECKOUT` names its checkout.

How the label's daemon takes a completion is one of those facts, and it is read the same way,
never by the label's name: `daemon-completion.go` reads the checkout's
`internal/workflow/engine.go` for how its engine answers a role that does not run the issue's
phase (`recorded`: `handoff` asks `leftItsPhase`, which records such a completion when the role's
row holds none, this checkout's; `refused`: `if RoleFor(issue.Phase) != fact.Role { return
refused("HANDOFF_NOT_CURRENT_PHASE", …) }`, an older daemon's, main `ff2a10ef`'s among them) and
`internal/api/handoff.go` for the phase its completion route holds a record to (`left`, the phase
the completion is of, `phase.Of`, this checkout's; `issue`, the phase the issue stands in, the
older daemon's). A source in neither shape, or in both, fails the read naming the file and what
it looked for, and a source that is not there fails it naming the file without running the reader.
The base label was main `ff2a10ef`, and is main at the commit the head's newest
forward merge brought in, which moves with every later forward merge, so a table keyed by the
label's name modelled a daemon the base checkout no longer had. The stand-in answers each
completion by the shapes it read at boot; `rig.sh health` and `rig.sh score` read the run's own
label's through the same facts cache (each is given the work directory), out of the sources of
the commit the run's `built-from` records (*What a reader reads*, above), and only for an answer
the two shapes do not give alike
(`completion-answers.ts`'s `ANSWERS`' `byShape`). A process reads each source once: the cache
keeps every reading that succeeded, and the process keeps each refusal, so a run whose daemon
cannot be read costs no more than the first run that asked.

What the label's daemon posts, refuses, publishes, ends a reviewer's task with, takes a review of
the review App for, records of each step and serves on its pull request is read out of its source
as text (`daemon-pane.ts`'s `labelShapes`), each as one of two shapes. A backward move's sentence
and the refusal of a backward move's reason are main's since #4309, read by `func
backwardMoveReason(` and `"BACKWARD_REASON_TOO_LONG"` in `effects.go`; main `ff2a10ef` predates
them, read by the status write it has in their place, which takes no message. A conflict's move,
main's from #4291 until #4464, is read no more: main's daemon keeps a conflicting head's issue in
its phase now, and no world's head conflicts (the declared-unreachable entry). The role holders'
publication is main's since #4228, read by
`"/role-holders"` in the daemon's Dispatch client and the route's row in Dispatch's table;
`ff2a10ef` is read by its outbox kinds, which end at `gated_steps_publish`. The two sentences that
end every task of the reviewer, `Last reviewed commit: <sha|none>.` and `Review entry: <n|none>.`,
are main's since #4328, read by `func namingTheReviewRound(` in `review_round.go` and by the three
words and the one return that join them in `reviewround.go`; `ff2a10ef` is read by its queue,
whose `enqueueAt` builds its row from the payload as it came. rig.sh's
`run_reviewer_text_findings` ends the reviewer's task with them (`bun daemon-pane.ts review-round
<checkout> '' 1`, the first round's: no commit, and the entry the reviewer's start raises), and
with nothing for a daemon of `ff2a10ef`'s shape. The rule that a review the review App submitted
decides the round only where its Legion footer names the session the reviewer's claim records,
and waits while that claim records none, is main's since #4328, read by `byReviewer`'s return and
`func (e *Engine) reviewersClaim(` in `review.go`; `ff2a10ef` is read by its `byReviewer`, which
lets the login decide while the reviewer's row names no claim, and by `claimSession`. The rule
that such a review decides only where the `head` its footer names is the pull request's head, 40
lowercase hexadecimal characters compared exactly, and is recorded against that head, is main's
since #2539, read by the footer's decoder in `intake/facts.go` (the `legionFooter` struct of a
`session` and a `head` string beside a raw `entry`, a decoding error costing the whole footer),
`LegionHead`'s `fullCommitSHA` and the two cases of `refusesRound` and `reviewHead` in
`review.go`; `ff2a10ef` is read by its footer decoder, which reads `session` alone, and by the
decision it records on the review's `commit_id`. The stand-in takes the run's reviews by both
(`daemon-standin.ts`'s `takenForRound`), each by the body the gh stand-in answered it with
(`made.body`): from that body's last footer it reads the session and the head the label's
decoder reads, and a footer that names no head, a prefix, the head in capitals or another head
decides nothing and leaves the record in `reviewing`. A review is judged at the first read that
finds it, and a taken one stays taken whatever the head comes to, as the engine keeps the
decision it recorded; a review the world held before the run is history it takes as it stands.
The head it is judged at is the remote's tip at that read, so a review production would refuse
at an older head, its webhook applied before the push's, and take at the reviewer's handoff
read, the world takes at the review: the same verdict, taken earlier (a residual, *Which of the
world's answers are pinned*, below).
The footer's `entry`, which moves the commit a later round's task names as `Last reviewed commit:
<sha>`, is not read: no scenario gives a reviewer a later round's task (the class search's
declared-unreachable entry). The three completion refusals (`HANDOFF_SESSION_MISMATCH`,
`HANDOFF_ALREADY_RECORDED`, `HANDOFF_NOT_NEW`) and the completion route's verdict rule
(`TESTER_VERDICT_REQUIRED` for a tester's verdict other than `pass` or `fail`,
`VERDICT_ROLE_FORBIDDEN` for any other role's, each in the words `handoff.go` holds) have one
shape: every daemon the rig runs names them. A source of neither shape, or of both, refuses the
batch at `label_facts` and every stand-in's boot, naming each marker and its file. A marker that
is not found is never read as an older daemon that lacks the behaviour. The refusal of a
completion that rates the plan outside the retro (`PLAN_RATING_UNEXPECTED`) is main's since #4488,
read by the route's request and fact in `handoff.go`, the engine's refusal past `HANDOFF_NOT_NEW`
with its fact left unspent in `engine.go`, its description and its rule in `workflow/plan.go` and
its words in `intake/facts.go`; `ff2a10ef` is read by its completion request, which ends at
`wakeAt`. The refusal of a request body a string of which holds U+0000 is main's since #4488 too,
read by `decodeJSON`'s last return and `refuseBadBody`'s words in `api/claims.go` and the walk and
its words in `api/storable_text.go`; `ff2a10ef` is read by its decoder, which ends at the one
value a body holds, by its engine's write of a completion's summary into the role's row and the
`phases` columns its store binds, by the move's reason in the task its transition starts with and
the JSON its outbox stores, and by its two `FACT_APPLY_FAILED` answers, the completion's in
`api/handoff.go` and every other fact's in `api/architect.go`. The count of fix attempts on the
state's pull request is `ff2a10ef`'s, read by
its `PullRequestView`'s `FixAttempts` in `api/state.go`, whose label's contract requires it; main's
since contract 59 (#2780) is read by that view ending at `reviewDecision`. `standin-record.ts
contract` hands a label's contract the bodies the stand-in serves that label, the count among
them where its daemon serves one. Both labels of a batch are now main `724196ef` or after, so each
reads as main's shape of all ten; the `ff2a10ef` shape of each is read by the rows below alone.

The rig's tests hold both shapes of each file to sources they write themselves, so they need no
older checkout on the host. Some rows also read an older daemon's own source where one is given:
`SKILL_SCENARIOS_OLDER_CHECKOUT` names a whole checkout of main `ff2a10ef` (2026-11-07), for
instance `git worktree add <dir> ff2a10ef` or an extract of `git archive ff2a10ef`, and
`daemon-standin.test.ts`'s "an older daemon's late completion is refused in that daemon's own
sentence" reads its `engine.go` and `handoff.go` as the older shapes and boots the daemon
stand-in on it, and its "an older daemon refuses no backward move's reason, posts no sentence and
publishes no role holder" reads a post with `daemon-pane.go`'s `post-check` built in that
checkout's module and boots the stand-in on it. `daemon-pane.test.ts`'s "the shapes of a label's
daemon source" reads it as the older shape of each reader, and refuses it with each older marker
renamed. The two stand-in rows need go and run `go run -overlay` of
`daemon-pane.go` in the checkout's `packages/daemon`, with its `packages/envoy` beside it (its
`go.mod` replaces that module), so the older module's dependencies must be in the Go module
cache or fetchable. With the variable unset the rows skip and say so. It is a test input only:
no batch reads it, and it is not the base label's checkout, which `SKILL_SCENARIOS_BASE_CHECKOUT`
names for the rows that read the base label's own contract.

### Which of the world's answers are pinned, and to what

The world claims to answer as production does only where a pin holds it to production. Each pin
is derived by a script in the rig from a reading of production, and keeps names, kinds, statuses
and error texts, never production's values, but for `production-headers.json`, written from the
implementer's reads of 2026-11-21, which keeps each read's `Date:` and the values of the headers
that describe GitHub's service rather than one answer (`Server`, `Content-Security-Policy`, the
edge that answers the pods, the rate limit of a role App, and the rest), which the world writes
too:

| Pin | What it holds | Read from | Made by |
| --- | --- | --- | --- |
| `production-shapes.json` | the exit, stderr and answer shape of 176 reads, and the line shapes of the 23 whose answer is text: the 5 reads of a branch a ruleset alone protects, the 90 distinct reads INFRA-764's round 25 made, the `read_record` record, the tester's 68 reads of each read the world served with no pin, the implementer's 9 reads that set does not hold, and 3 of the tester's 21 readings again, whole, of every text read, whose other 18 replace the earlier readings of their reads | cybermonklabs/legion `main` at 2026-11-19T05:35:00Z; its pull request #2827 at about 15:40Z on 2026-11-17, and a Legion issue pod's own record at 16:49Z that day; the open #2827, the merged #4326 and the repository at about 08:45Z (the tester) and 08:55Z (the implementer) on 2026-11-18, and every text read again at about 17:22Z (the tester) | `production-shapes.ts` |
| `rest-required.json` | the required and declared field tree of the success answer of each REST operation the world serves: each object's required fields and, where it takes no other, every field it declares (`keys`) | github/rest-api-description at `DESCRIPTION_COMMIT` (`c9c95fe34ced`) | `rest-required.ts` |
| `production-headers.json` | the status line, the header names in gh's order and each header's value but those that describe one answer (`<the answer's own>`) of 9 `gh api -i` reads — the repository, with and without `--jq .visibility`, a 404 of a served route and of an endpoint GitHub does not serve, a GraphQL query, a paged list, and a commit's diff and SHA and a file's raw bytes — each with its `Date:`, and the permission (`X-Accepted-Github-Permissions`) production named for the read of each of 30 REST routes the world serves | cybermonklabs/legion from 10:20:07Z to 10:35:20Z on 2026-11-21, read with gh 2.98.0 and a role App's installation token | the implementer, from those reads; no script of the rig |
| `graphql-schema.json` | the field and return type of each GraphQL type the world serves, which the world's query check reads | GitHub's introspection at 2026-11-17T21:20:23Z | `graphql-schema.ts` |
| each label's own `LegionStateResponse` | every state body the daemon stand-in can serve, parsed before the label starts | the label's own checkout | `services_up`'s `label_contract` |

A read whose answer is text is pinned by its lines too, not by the kind `string` alone, which
an empty or wrong-format answer passed (`production-shapes.ts`'s `lineShapes`). The text views
(`gh pr view`, `--comments`, a `gh run` refusal's empty stdout), a diff, `--name-only`, the rows
of `gh pr list` and `gh pr checks`, `gh auth status` and the `sha` media each have a layout,
chosen by the read's command. Each line keeps gh's and git's own words: a `<key>:\t` prefix,
`--`, `diff --git`, `@@`, the fixed sentences of `gh auth status`. Every value is a placeholder.
A row is its field count (`<5 tab-separated fields>`), and the body after a view's `--` is one
`<body>`. The shapes are a set, so how many lines of one shape a text holds is no shape. A
production line no rule reads fails the derivation, and so does a value of production's in a
line. Each text answer is pinned from a reading of it whole: the tester read every text read
again on 2026-11-18 (`prod-text-reads.json`), and a reading again of a read an earlier set holds
replaces that reading, under its name. The derivation refuses a text answer cut short: one its
set's README says was cut, or one of 600 characters with no line end, the length round 25's
store cut its text answers at. The rows read the world's text as gh wrote it, untrimmed, and
name each difference of state with its reason: the world's diff adds files and removes no line,
where #4326's removes lines, and #2827's diff, past GitHub's 20000-line limit when read whole,
is refused (HTTP 406), so the world's is held to #4326's.

The issue workspace's git configuration is held line for line to a Legion issue pod's clone, as
read on 2026-11-17 (the paragraph on a worker world's issue workspace, above), and the base's
ruleset to production's seven rules (*The base's ruleset*, below). Some answers rest on one live
measurement each, named where the answer is described: a reply to a reply filed in its thread,
one App's update of another's review, GitHub's 404 body for an endpoint it does not serve, and
the merge state of an approved, green head on a merge-queue base. A few are stated as
unmeasured: GitHub's 404 for another App's read of a pending review by id, and the
rule-violation lines of a refused push beyond the three measured. Eleven differences are named
residuals rather than defects, each an answer the world cannot give as production gives it:

- `remote.origin.vcs`, the key that carries the world's remote. No production clone has it. The
  world's remote is the run's own, so a clone of it needs a transport of the world's.
- A review's `url`, which gh asks for only past a pull request's 100th review. No world's pull
  request holds 100 reviews.
- `gh auth status -t` (`--show-token`) and `gh auth token`. Production prints the pane's token.
  No stand-in reads a token, so the world refuses both calls (`gh: Not Found (HTTP 404)`, exit 1).
  A run that makes either is unhealthy (`rig.sh health`'s rule 21, `credential printed`), and the
  tester reports it as an agent printing its credential.
- GitHub's `406 PullRequest.diff too_large` for a pull request whose diff is over 20,000 lines,
  which production gives `gh pr diff` on #2827. The world's pull request is small, as #4326 is, so
  its diff reads are held to #4326's readings with the state difference named
  (`PINNED_GH_STATE`), and no scenario reaches the refusal.
- The daemon's normalising of a session's capability report (`capabilities.Normalize`: the live
  table's order, a name it does not know dropped, an unreported row marked open, and
  `cleanDetail`'s redaction of credentials and control characters and its 1024-byte cut), which
  the daemon stand-in does not model (the architect's ruling of 2026-11-19T05:18Z). Round 25's 69
  agents each sent only the boot's `claims/ready`. A second is reachable only by an agent idle at
  a 5 s tick 30 s or more after its registration (`packages/pi-legion/extensions/legion.ts:705-712`,
  `capability-schedule.ts`), and the redaction is code no marker reads. A run whose claim sends one
  is unhealthy from that call on (`rig.sh health`'s rule 22, `capability report`, naming the line
  and the claim), since the world's capability rows are not production's after it. If a batch
  loses a run to it, that count buys the model.
- The order in which production applies a review's webhook and its push's. The world reads the
  remote's tip live (`daemon-standin.ts`'s `worldPull`), so a review whose webhook production
  applies before the push's is taken at the review in the world and at the handoff read in
  production: main's daemon judges a review App review at the head the record holds when its
  webhook is applied, refuses that one there (`RefusedAtThisHead`), and judges it again by the
  per-issue read at the reviewer's handoff (`issue_reads.go`'s `readHandedOff`, then
  `review_reads.go`'s `reviewToApply`). The verdict is the same, and k1 scores the verdict (the
  architect's ruling of 2026-11-19T09:42Z).
- The merge state, which the world answers alike to every viewer (`gh-threads.ts`'s
  `mergeStateOf`). Production gave the review App `BLOCKED` and the implement App `CLEAN` on
  cybermonklabs/legion#4461 (reads of 12:55Z to 13:00Z on 2026-11-19, head `21dc91c7`, carrying
  three review App commits); the pull request then merged through the queue at 14:02:33Z as
  `73e417f113`. GitHub's rule behind the per-viewer answer is not known, so it is not modelled.
  Reviewers and the implementer can see different states in production only where a reviewer
  reads the merge state after its own approval. If a batch loses a run to it, that count buys the
  model (the architect's ruling of 2026-11-19T14:07Z).
- `legion gh`'s refusal of a write whose repository GitHub answers 404 to the leak guard's read of
  its visibility, exit 2 before gh runs, saying that GitHub answers 404 for the repository (main's
  since #4551, leakguard's `NotFound`, where the guard's setup error refused it before). A write
  naming another repository of the world's owner passes the world's owner refusal
  (`gh-standin.ts`'s `ownerRefusal`, which refuses another owner alone), meets no route, since a
  write's route reads its endpoint whole, and is answered `unrouted`: `gh: Not Found (HTTP 404)`,
  exit 1. A run that makes one is unhealthy from that write on (`rig.sh health`'s rule 20,
  `unmodelled write`), and the run is replaced (the architect's ruling of 2026-11-20T10:05Z). No
  other write meets the refusal: the world's own repository exists, so the guard's read of it is
  answered `public` (`fixture_write`); a write naming another owner is refused exit 2 before any
  route reads it; and the world's repository is no fork (`fork:false`), so the credential helper's
  answer for a fork (`forkUpstreamDir`) is never asked.
- GitHub's test merge (`refs/pull/<n>/merge`) of a head that does not merge into its base, which
  GitHub keeps from the last time the head merged (`git/ref/pull/4532/merge` answered 200 while
  `pulls/4532` gave `merge_commit_sha` null, 08:45:31Z on 2026-11-21) and the world answers
  none of. The base never moves, but the world's remote takes a force-push of another history to
  the issue branch, as GitHub does for an unprotected branch. A run whose push leaves a head
  without the base's tip in its history is unhealthy from that push on (`rig.sh health`'s rule
  24, `unmerged head`), and the run is replaced.
- A head that conflicts with its base, and everything main's daemon does about one since #4464:
  the second read that confirms the conflict, the messages asking the implementer to merge the
  base forward and the phase's worker to stop writing, the refusal of a completion at the head a
  merge forward replaced, the merge-state read a push asks for, and the review asked once the
  merge lands (`daemon-standin.ts`). An agent reaches it by a forced push to the issue branch: the
  world's `proc-receive` hook (`fixture_first_commit`) checks no fast-forward on any branch but
  the base, and on a scratch copy of run 94caf2e09844's remote a forced push of another history
  answered `(forced update)`, exit 0, and `git merge-tree` of the base and that head then refused
  to merge unrelated histories, exit 128. The world still reports such a head's merge state as
  one that merges. A run whose push leaves a head without the base's tip in its history is
  unhealthy from that push on (`rig.sh health`'s rule 24, `unmergedHead`, which names the first
  such push), and the run is replaced (the architect's ruling of 2026-11-21T10:17:43Z, which
  re-filed it from the entries no world reaches).
- Two headers of `gh api -i` (`production-headers.json`): `Last-Modified` where its time is none
  the world's objects give — production sent the commits, refs, trees, tags and contents one each,
  and `git/ref/heads/main` answered the repository's `updated_at` where `git/refs/heads/main`
  answered the commit's, at 10:24:58Z and 10:25:00Z on 2026-11-21 — and
  `X-Accepted-Github-Permissions` on a write, which no pod makes in production to read its
  headers. The world writes neither there; it writes `Last-Modified` for the repository and the
  pull request and its commits and files, each production's own reading of it.

**A read with no pin, and an answer this section and the sections below do not name, is not
claimed to answer as production does.**

### What the gh world keeps, and where it stops

The gh stand-in answers GitHub from the world's seed and the effect of every call the run made,
read from the run's own `calls.jsonl` (`gh-threads.ts`, one replay for the gh stand-in and the
daemon stand-in). A thread a review's inline comment or a comment of its own opens is listed by
`reviewThreads` and by the REST review comments; `resolveReviewThread` reads back
`isResolved: true`; a reply, through GraphQL, the REST reply route or the `in_reply_to` of a new
comment, is its thread's last comment; and each comment's `databaseId` is its REST id.
`score.ts` reads the same replay for what each call did to a thread: the threads the world
resolved at it, and each reply it filed, by the thread and the body the world filed, never by the
node ids its text names. Every world's base is protected as
production's `main` is, by one repository ruleset alone holding production's seven rules
(`rig.sh`'s `base_ruleset_routes`; what the world does with each is the next section), and the
pull request's review decision, merge state and mergeability are computed from those rules, the
reviews, the head's checks and the threads as the run leaves them, and the daemon stand-in moves
the record on that same state. The rules of the branch, the repository's rulesets and
the ruleset by its id (GitHub's 404 for any other id) answer from it; the branch read answers the
branch at its head on the run's remote, `protected: true` with the classic protection off and
naming no check; and the classic protection's own endpoint (`…/branches/<base>/protection`) is
GitHub's 403 to a role App, whose token may not read a repository's administration: `gh: Resource
not accessible by integration (HTTP 403)`, GitHub's error body on stdout, exit 1, with or without
a `--jq`, which gh runs over no error. The check runs
are reported on the commit read (its `head_sha`, its App — GitHub Actions for `test` and `lint`,
GitHub Advanced Security for `CodeQL` — and their times counted
from when that commit reached the run's remote). A `contents` read answers the file or directory
at the ref it names, or on the default branch where it names none, raw or as GitHub's JSON.
The run's remote answers GitHub's other reads of its objects too, each a route of `fixture_write`
read in `gh-standin.ts`'s `fromRun`: a commit by any ref GitHub reads, a branch with a slash in
its name among them and a short SHA answered with the full one, with its stats and its files
(the diff from its first parent, paged 300 a page), or its diff, patch or SHA under those media
types, and GitHub's 422 `No commit found for SHA: <ref>` with its body for a ref the remote
cannot resolve; the commits a ref reaches, newest first and paged (a filter by path, author,
committer or time is refused as a read the world does not model); a commit's git data, by its
full SHA alone; a tree, recursive for any value of `recursive`; the branches, the base alone
protected; the tags, none in any world; the pull request's commits, the ones its head holds and
its base does not, oldest first, and its files, the comparison's of its base and its head, each
list paged (GitHub's 404 of the list, its body naming the list's page, for a number the world
holds no pull request of); and the refs, the remote's and the two GitHub keeps for
the pull request (`refs/pull/<n>/head` at its head, `refs/pull/<n>/merge` at its test merge),
one by its name and a list by a prefix of names. A ref, a tree or a list the remote cannot answer
is GitHub's 404, its body naming GitHub's page of the read, and so is a comparison of two dots
(`<base>..<head>`), which GitHub does not read as one, or of a ref the remote has not got. Each
answer is the one GitHub gave on 2026-11-21, read with `gh api -i` against cybermonklabs/legion;
`gh-standin.ts` names each read and its `Date:`. GitHub's test merge of a head that does not
merge into its base is a residual (*Which of the world's answers are pinned*, above).
`gh api -i` (`--include`) prints GitHub's status line and headers before each page's body, as gh
prints them (the protocol and the status on a line of their own, each header `Name: value` in the
byte order of the names, ended `\r\n`, then a blank `\r\n`), on every route the stand-in serves,
and `--jq` runs over the body alone. The status is the answer's: the operation's success, `201
Created` for a create of a comment and `200 OK` for every other, or GitHub's refusal's. The
headers are the ones production sent the same class of answer (`production-headers.json`): the
service's own with production's values, and `Date`, `Etag`, the request id, `Last-Modified` and
the rate limit with the world's (the call's time, a validator of the world's body, an id in
GitHub's form, the world's object's time, and the pane's App's limit counted from the run's own
`gh api` calls of that resource); a REST read's `X-Accepted-Github-Permissions` as production named
it for that route; a list's `Link` to its other pages; no rate limit, API version or permission
on an endpoint GitHub does not serve, and no caching, ETag or permission on a GraphQL answer. Two
of them are residuals (*Which of the world's answers are pinned*, above).
A call naming an owner outside the world's project is refused as the Legion gh wrapper of the
run's label refuses it, for every role: exit 2 and the line the wrapper prints for the form that
names the owner, never a 404. The world reads the call by the wrapper's own selector table
(`legion gh`, `packages/daemon/cmd/legion`: `ghSlotKinds`, `ghOptionKinds`, `ghLeafQuirks` and
`ghKindRank` in `gh_help.go`, read with the wrapper's own patterns of a value), read out of the
label's checkout by `gh-selectors.go` at `services_up` and kept in the facts cache, joined with
the gh help that checkout saves for the wrapper's tests (`testdata/gh-help`), and written into
each worker run by `worker_boot`, since the agent's namespace hides the checkout
(`gh-selectors.ts`). A form naming one repository of that owner and no other (`gh repo view
actions/checkout`, `gh release view v7.0.1 -R actions/checkout`, `gh api
repos/actions/checkout/…`, a pull request's URL) gets the leak guard's line, `legion gh: LEAK
CHECK SETUP ERROR: the gh call: the visibility of <owner>/<repo> could not be read: gh api
repos/<owner>/<repo> exited 2: legion gh: "<owner>" is not an owner of this project:
<LEGION_GH_OWNERS_DIR> holds no gh directory for it`; a form naming the owner alone (`gh search
repos checkout --owner actions`, `gh repo list actions`, `gh api orgs/actions/…`) gets the last
part of it alone, `legion gh: "<owner>" is not an owner of this project: <LEGION_GH_OWNERS_DIR>
holds no gh directory for it`, since the guard has no repository to read; and a call naming the
world's owner beside it, its own repository read from the pane's `GH_REPO` where gh reads it,
gets the wrapper's `the command names more than one owner, …`. A form the label's wrapper holds
that this reading does not model fails the batch at `services_up`, naming it, and so does the
pin in `gh-standin.test.ts`, which runs every form the table holds through the world.

A wrapper of main's since #4035 (cac1693d) refuses fewer of these calls: a call that writes nothing
(`ghWritesNothing`, or a `gh api graphql` query `ghGraphQLQueryRead` admits, in `gh_reads.go`),
names only owners the pane's bundle holds no gh directory for, and reads no working-directory
remote for its own repository, runs with the pane's own `GH_CONFIG_DIR` (gh.go's `fallback`,
gh_selectors.go's `dir`). The world reads which shape the label's wrapper is off its source, by
marker, as it reads the daemon's (`daemon-pane.ts`'s `PANE_TOKEN_READ` and
`OWNER_ALWAYS_REFUSED`; a source of neither shape fails the batch, naming both), and reads the
wrapper's read grammar out of the same checkout with its selector table (`gh-selectors.go`'s
`read`, `api-read`, `graphql-read` and `override-header` lines). On a wrapper of that shape, from a
pane that names its `GH_CONFIG_DIR`, as every worker pane does, such a call is no refusal: it goes
on to the world's GitHub as any call does and gets what the world answers for that owner
(`gh-pane-token.ts`'s `runsOnPaneToken`). The world holds the repository of its own project alone,
so a read of another owner's repository gets GitHub's 404, exit 1, as GitHub answers a token
that cannot see a repository (`gh repo view actions/checkout`, `gh api
repos/actions/checkout/git/refs/tags/v7.0.1`, `gh search repos checkout --owner actions`, `gh repo
list actions`), and a GraphQL query of such an owner its answer for one it does not hold
(`repositoryOwner` null). A write naming such an owner, a read typed outside the wrapper's read
grammar (`gh repo view actions/checkout --web`) and a call naming the world's owner beside another
are refused as above, and so is every such call on a wrapper before #4035. Health declares that
404 for every role as it declares the refusal (`OWNER_UNHELD`).

The world's reads are held to production's own answers by `production-shapes.json`: for each
read, the command it was made with (production's values written as placeholders: `<owner>`,
`<repo>`, `<number>`, `<head>`, `<base>`, `<review>`, `<comment>`, `<path>`, `<id>`, `<branch>`,
`<app>`), when it was read, its exit, its stderr and its answer's shape — each JSON path and the
kind of the value there, a list's items read per variant (a rule's `type`) where they differ — and
no value of it. It pins 176 reads, each made in an issue pod, a gh read with the role App token
its gh gives:

- the five reads of a branch a ruleset alone protects (`rules/branches/main`, `rulesets`,
  `rulesets/<id>`, `branches/main` and `branches/main/protection`), read of cybermonklabs/legion
  at 2026-11-19T05:35:00Z, after its ruleset came to require a code owner's review; they replace
  the reads of 2026-11-17T15:59:18Z, whose shapes they share, so the pin moved by those five
  reads' `read_at` alone;
- the 90 distinct reads the 44 runs of INFRA-764's round 25 made, read against a pull request of
  the same repository at about 15:40Z on 2026-11-17;
- the issue record below, read at 16:49Z that day;
- the tester's 68 reads of each read the world served with no pin (`prod-reads-19.json`, in
  version 2 of the `infra-764-production-reads-tgz-b64` upload), read at about 08:45Z on
  2026-11-18 against #2827, then open and conflicting, #4326, merged, and the repository;
- the implementer's 9 reads that set does not hold (`implementer-reads.json`, uploaded as
  `implementer-r26-w10-state-tgz-b64`), read at about 08:55Z that day: a commit by its SHA and a
  commit's pull requests, as Dispatch reads them, `gh run view` and `gh run watch` naming no run,
  the viewer by the implement App's token, and GraphQL sent to `gh api /graphql` and `gh api
  'graphql?…'`;
- the tester's 21 readings again, whole, of every text read (`prod-text-reads.json`, in version
  3 of the `infra-764-production-reads-tgz-b64` upload), read at about 17:22Z that day. 18 read
  again a read pinned above, and replace its reading under its name: the five round 25 kept cut
  at 600 characters, #2827's two text views and its three diff reads, which GitHub now refuses
  as past its 20000-line limit; the twelve text reads of `prod-reads-19.json`; and round 25's
  GraphQL query that is no document. 3 are new: `gh pr diff --color never` and the two text
  views of #4326, a pull request with no comment on its conversation.

`production-shapes.ts` derives it from those reads' own files, which stay out of the tree, and
refuses to write a value of theirs into it. With the uploads
`implementer-production-ruleset-reads-2026-11-19-tgz-b64` (dispatch://INFRA-764, its tgz's sha256
`78d9b6d248c3…`, which unpacks to `infra-764-production-ruleset-reads`), version 3 of
`infra-764-production-reads-tgz-b64` and `implementer-r26-evidence-tgz-b64` (which holds
`implementer-reads.json` too) each unpacked in `<uploads>`, this, run in
`packages/pi-legion/scripts/skill-scenarios` so that biome reads the repository's configuration,

```sh
bun production-shapes.ts <uploads>/infra-764-production-ruleset-reads \
  <uploads>/infra-764-production-reads/infra-764-production-reads.json \
  <uploads>/production-read-record.json \
  <uploads>/infra-764-production-reads/prod-reads-19.json <uploads>/implementer-reads.json \
  <uploads>/infra-764-production-reads/prod-text-reads.json production-shapes.json
```

writes it and formats it with the repository's biome; a second run writes the same bytes. A row
holds a world's answer to a pinned
read with `shapeDiff`, which reports the paths production has that the world lacks, the paths
the world has that production does not, and the paths whose kinds differ, none of them below a
path whose kind differs. An array the world holds empty is not compared below itself, nor is a
variant of a list the world holds none of, since how many items a list holds is the world's state
and not its shape; an array production held empty pins no item shape, so an item the world holds
there is a path production does not show.

Every pinned read is compared by a row, which makes production's own command with the world's
values in it:

- the five ruleset reads, each with no difference, and each of production's seven rules to
  production's rule of its type, none of them absent from the world (`gh-standin.test.ts`);
- the 117 reads of gh's own commands, `gh pr view`, `list`, `diff` and `checks` and `gh run view`
  and `watch`, 55 of round 25 and 62 of 2026-11-18 (`gh-standin.test.ts`'s `PINNED_GH`), and
  #4326's two text views again in a world no agent has commented in;
- the 28 GraphQL reads, 20 of round 25 and 8 of 2026-11-18 (`PINNED_GRAPHQL`);
- the 22 REST reads, 15 of round 25 and 7 of 2026-11-18 (`PINNED_REST`);
- `gh auth status`, in its own row;
- the two reads of Dispatch's alone, a commit by its SHA and a commit's pull requests, asked of
  the run's GitHub stand-in, which the same row holds to the pins of the repository and the pull
  request too (`PINNED_HTTP`);
- the issue record, in `daemon-standin.test.ts`.

The row `every read the pin holds is compared by a row` fails naming a pinned read no row
compares. Until 2026-11-18 the 20 GraphQL reads of round 25 were pinned and no row compared one.

A difference of state is named in its row, with the reason production's reading gives:

- A review's `url` in `gh pr view --json reviews`: gh asks GitHub for it only on the pages after
  its first 100 reviews, so production's #2827, of 137, holds it on its last 37 and no world, of
  fewer than 100, holds it (measured on 2026-11-17 on #2827, whose reviews from the 100th on carry
  it and no earlier one does, and on #4326, whose 2 carry none).
- `potentialMergeCommit`: #2827 conflicted with its base when read (`mergeable` `CONFLICTING`), so
  GitHub held no test merge and gh read null. The world's head merges cleanly, and its test merge
  is an object.
- `statusCheckRollup` and `gh pr checks` of #2827: its head had reported no check run, so the
  rollup was empty and `gh pr checks` refused it (`no checks reported on the '<branch>' branch`,
  exit 1). Every world's head reports its checks, so the world's answers are held to the tester's
  readings of the same commands on #4326, whose head had them.
- #2827's diff, read whole: it had grown past GitHub's 20000-line limit, so GitHub refused it
  (`HTTP 406`) and gh printed nothing, exit 1. The world's pull request changes a few files, so
  its three diff reads are held to the tester's readings of the same commands on #4326, less the
  difference below.
- A diff's lines: #4326's changes files that were there and removes lines of them, and the
  world's adds its files (`new file mode`) and removes no line.
- A commit's pull requests: the implementer read #4326's head, whose pull request was merged, so
  its `closed_at` and `merged_at` were times. The world's pull request is open, and GitHub answers
  an open one's null, as the REST read of the open #2827 holds them.
- A commit's check runs: production read them while they ran (below).

The world's values fill the placeholders: `<app>` is the implement App's slug, the App whose pull
requests production's `gh pr list --author app/<app>` listed, and a directory read is the world's
`.github`, which holds a directory alone, as production's `.github/actions` held directories
alone (a directory's `download_url` is null and a file's a string). A GraphQL read made with
`--paginate` is held as its pages, the form production's was kept in: #2827's threads, more than
100, came on two pages, which gh printed back to back, and the world's come on one. GitHub's place
in a refused query that names the repository is five columns less in the world than in
production (`at [1, 171]` for `at [1, 176]`), since the world's owner and repository are five
characters shorter.

The branch's head is the world's developer's commit, signed with an SSH key of the developer's
own: the world's GitHub holds the developer's account by the address the commit carries and the
key it signs with (`rig.sh`'s `fixture_first_commit` writes `<run>/accounts.json` and
`<run>/allowed-signers`), so the commit's `author` and `committer` are that account and its
`verification` is the one `git verify-commit` gives against that key, as production's head is
linked and verified.

### The base's ruleset: what each rule does in the world

The world's ruleset holds the seven rules production's `main` holds (cybermonklabs/legion ruleset
12331919, read 2026-11-19T05:35:00Z, `production-shapes.json`'s three ruleset reads; the reading
of 2026-11-17 differed in `require_code_owner_review` alone, false then, and its `updated_at`), in
production's order, each with production's parameter keys in production's value kinds and at
production's values, since a rule's parameters are the deployment's configuration and no value
that names production; the exceptions are the checks and the code-scanning tool, which are the
ones the world's head reports, and `required_review_thread_resolution` where a world's premise
asks for it. The world's `rulesets` list holds this one ruleset: production's lists two,
the one on `main` and one on another branch, and the world's repository protects its base and
no other branch, so its list is what a repository holding production's `main` ruleset alone
answers, in production's item shape. Each rule is either **modelled**, its effect computed in
the world, or **shown only**, answered by every ruleset read and acted on by nothing. The text of
a rule, or of a parameter, with no modelled effect is not claimed faithful: an agent that acts on
it meets nothing of the world that does.

- `deletion`, `non_fast_forward`, `required_linear_history` — **modelled** at the run's remote
  (`rig.sh`'s `fixture_first_commit`): once the base is pushed, its `proc-receive` hook takes every
  push command and refuses each on `refs/heads/<base>`, printing `remote:` lines that name each rule
  the push breaks and git's ` ! [remote rejected] <src> -> <base> (push declined due to
  repository rule violations)`, exit 1; a push to any other branch is taken, but for a workflow
  change by an App without `workflows`, which the hook refuses first, on the base too (the
  remote's answer to each App, under *skill-scenarios/rig.sh* above). A deletion breaks
  `deletion` (`- Cannot delete this branch`); an update breaks `non_fast_forward` where the
  base's tip is not its ancestor (`- Cannot force-push to this branch`), `required_linear_history`
  where a commit it adds is a merge (`- This branch must not contain merge commits.`, then `Found
  1 violation:` or `Found <n> violations:` and each merge), and, since the base changes only
  through a pull request its queue merges, `pull_request`, `required_status_checks` and
  `merge_queue` whatever it carries.
  Measured (INFRA-2713's sandbox, 2026-11-12, a direct `git push origin HEAD:main` to a base whose
  ruleset required a pull request, a check and a merge queue): the first line, `error: GH013:
  Repository rule violations found for refs/heads/<base>.`, and `- Changes must be made through a
  pull request.`, `- Required status check "<name>" is expected.` (one check) and `- Changes must
  be made through the merge queue`. Unmeasured, GitHub's form as public reports of a rejected push
  print it, since no read-only call makes GitHub refuse a push: the review link (`Review all
  repository rules at http://github.com/<repo>/rules?ref=refs%2Fheads%2F<base>`), the lines of the
  three branch rules, `- 2 of 2 required status checks are expected.` for the world's two checks,
  and the rejection reason. GitHub's pushes print nothing of `code_scanning` here, and the world's
  neither (unmeasured).
- `pull_request` — **modelled**: `required_approving_review_count` (1) decides the review decision
  (`gh pr view --json reviewDecision`, GraphQL `reviewDecision`) — `CHANGES_REQUESTED` while any
  reviewer's last deciding review requests changes, `APPROVED` once an approval stands,
  `REVIEW_REQUIRED` before — and holds the merge state `BLOCKED` until it is `APPROVED`
  (`gh-threads.ts`'s `reviewDecisionOf`, `mergeStateOf`); `dismiss_stale_reviews_on_push` is
  modelled at production's value, false: an approval stands across a push, as production's does
  (a value of true is not modelled);
  `require_last_push_approval` is false, production's value, in every world but `last-push`'s,
  where it is true and an approval by the login that made the head's last push is not counted
  ("Whether the most recent reviewable push must be approved by someone other than the person who
  pushed it", GitHub's REST description of the rule; the login is the account the world links the
  head commit's committer address to, `rest-objects.ts`'s `pusherOf`);
  `required_review_thread_resolution` (true where a world's premise is a head
  its open threads hold `BLOCKED`, `resolution_protection_routes`; false elsewhere and in
  production) holds the merge state `BLOCKED` while a thread holding a published comment is open;
  `require_code_owner_review` (true, production's since 2026-11-18T18:19:22Z) asks that "any pull
  request that modifies content with a code owner must be approved by that code owner"
  (docs.github.com, Available rules for rulesets), and content has a code owner only where the
  base's `.github/CODEOWNERS`, `CODEOWNERS` or `docs/CODEOWNERS` names one (About code owners): no
  world's repository holds one (the architect's ruling of 2026-11-19), so it asks no approval of
  its own and the decision is the count's, as `gh-standin.test.ts`'s code owner row holds; a world
  given a CODEOWNERS file would need its owners read, which nothing of the world does.
  Shown only: `allowed_merge_methods` (`squash`; the world merges nothing, `gh pr merge` being a
  write it refuses), `dismissal_restriction`, `required_reviewers` (none) and
  `require_extra_approval_for_unattributed_changes`
  (true, as production's): GitHub's REST description asks that extra approval only of "a pull
  request Copilot opens without a human collaborator", and every world's pull request is the
  implement App's, so at production's value it changes no modelled effect. The daemon stand-in's
  record keeps main's own `reviewDecision`, the
  decision of the review that last decided the round (`api.PullRequestView`), which reads as gh's
  wherever the rows compare them: gh's `REVIEW_REQUIRED` is the record's absent decision.
- `required_status_checks` — **modelled**: each check it names (`test`, `lint`, by GitHub
  Actions' integration id 15368) holds the merge state `BLOCKED` until the head has a run of that
  name by that App that completed with `success`, `neutral` or `skipped`; a run still going, one
  that failed and one never reported are none. Shown only: `strict_required_status_checks_policy`
  (false; a head behind its base is the world's own `BEHIND` state, `mergeStates`) and
  `do_not_enforce_on_create`.
- `merge_queue` — shown only, every parameter, at production's values (`merge_method` `SQUASH`,
  `max_entries_to_build` 5, `min_entries_to_merge` 1, `max_entries_to_merge` 4,
  `min_entries_to_merge_wait_minutes` 0, `grouping_strategy` `ALLGREEN`,
  `check_response_timeout_minutes` 180): the world holds no queue and enqueues nothing. The one
  effect it has on what an agent reads is the state of an approved head: measured on 2026-11-18
  at 04:19Z on cybermonklabs/legion, whose `main` queues, its open pull requests #4346, #4334,
  #4328 and #4325, approved with `ci-ok`, `pr-title` and `CodeQL` successful, read
  `mergeStateStatus` `CLEAN` and `mergeable` `MERGEABLE`, and a world's approved, green, resolved
  head reads the same.
- `code_scanning` — **modelled**: each tool it names (`CodeQL`) holds the merge state `BLOCKED`
  until the head has a check run of the tool's name that concluded `success`. Every world's head
  reports one (`rig.sh`'s `concluded_checks`), as production's heads carry CodeQL's: a `CodeQL`
  check run of GitHub Advanced Security's App, completed `success`, titled `No new alerts in code
  changed by this pull request`, its link GitHub's page of the run (measured on 2026-11-18 on
  #4346's head). Shown only: `security_alerts_threshold` and `alerts_threshold` (production's,
  `high_or_higher` and `none`); the world raises no alert.

`mergeable` follows the merge state, as production's does (measured on 2026-11-18 over the 60
newest open pull requests): `CONFLICTING` for `DIRTY`, `UNKNOWN` for `UNKNOWN`, `MERGEABLE` for
every other state; the REST `mergeable` is its boolean, null while `UNKNOWN`. A state a world
writes for a head (`mergeStates`: `DIRTY`, `BEHIND`, `UNKNOWN`) stands over every rule, as
production's #4374 read `REVIEW_REQUIRED` and `DIRTY`.

### The issue record a `read_record` answers

The daemon stand-in answers `GET /legion/v1/state`, the `legion` tool's `read_record`, with
production's record of the run's issue (`standin-record.ts`, built from what the run's world
says at each read), field for field as the daemon's projection builds one. Until INFRA-764 round
25 it held 8 keys and `workers: {}`, so a threads-closed implementer that read it before its
first write saw no row of its own completed round. It holds:

- `architect` and a row of `workers` for each role the world's history ran and each agent the
  run booted. The history is read back from the run's own Dispatch at the stand-in's boot, by
  the records `<run>/fixture.json` names (the seeds of `worker_run_world` and its runners): the
  architect is the session that filed the issue; a role's session is the author of its seeded
  handoff record; its `handoffRecord` is that record's id and version; and `handoffCommit` is, for
  the implementer and the tester, whose completions name one, the `head` that role's own seeded
  record names: the implementer's commit, below the reviewer's text commit where the world has
  one, since the reviewer's push is no step of theirs; it is absent for the planner and the
  reviewer, as on production's rows. A seed the run's Dispatch
  does not hold as that issue's record of that phase and version stops the boot, naming it. The
  booted agent's claim carries the session it registered, `launching` until it registers,
  `registered` until it says it can be prompted, `working` from then until its turn's outcome
  and `idle` after it, with the times of each; the second agent of a run reads the first's claim
  off the run's `daemon.log`. Each claim's `locator` is production's sandbox locator for the
  run's own issue pod (`legion-<project>-<issue>`, the pod's uid drawn from the two), its
  generation the order the pod's processes started in.
- `pendingTask` on the claim the daemon queued a task to: the booted role's where the daemon
  started its phase (tester-proof's tester, reviewer-text-findings' reviewer) until its turn
  ends, none for threads-closed's two halves, whose loop the daemon starts no phase for, and the
  role's that a move of the record starts.
- `pullRequest` as gh answers the world's pull request at that read: its number, the head its
  branch is at on the run's remote, its state, its merge state as the run's review threads leave
  it, and the decision of the review that last decided the round, of the reviews the label's
  engine takes: a person's on its `commit_id`, and the review App's reviewer's own only where the
  `head` its body's Legion footer names is the pull request's head, which that decision then
  stands on (the shapes of *Where each service of a run comes from*, above); `fixAttempts`, 0,
  for a label whose daemon counts a pull request's fix attempts (main `ff2a10ef`, whose contract
  requires it), and none for one whose daemon keeps no count (main's since contract 59); and
  `mergeWait` while an approval of the head waits in `reviewing` on a merge state the issue may
  not advance on.
- `designGate` and `gatedSteps` as the run's Dispatch holds the issue's spec at that read: its
  document, its latest version and the version a person approved. The seed approves the spec
  (`seed.ts`'s `testerProofIssue`: the issue is its tree's root, whose work waits behind that
  gate), so a world's gate reads approved at version 1, and a later write of the spec reads
  `waiting`. A read the run's Dispatch does not answer for the spec is answered 500 with why.
- `slot` while the issue holds one (none in `accepting`, as an issue past its review holds none,
  and `admission.active` lists it accordingly), `phaseEntry` and `givenEntry` of the role working
  the phase, the status its phase shows on Dispatch (`testing`, `needs_review`, `in_progress`),
  and `treeMovedAt` with `quietHours`, counted from the last request of the claim's the stand-in
  took or the last move of the record.

The record moves as the stand-in's served phase moves: a request for changes sends the issue to
`implementing`, raises the implementer's `pass` and `rounds`, counts
`backwardMoves["reviewing-implementing"]`, clears its handoff and queues its task; a worker's own
move back counts its pair and no round; an approval of a mergeable head moves the issue to
`accepting`, gives up the slot and queues the architect's task; and a completion the stand-in
takes is written on its role's row. A head that conflicts with its base is not modelled: main's
daemon since #4464 keeps the issue in its phase, confirms the conflict by a second read, asks the
implementer to merge the base forward and the phase's worker to stop writing, and refuses a
completion at the head the merge replaced (`HANDOFF_HEAD_MERGED_FORWARD`). It is a residual (the
architect's ruling of 2026-11-21T10:17:43Z, *Which of the world's answers are pinned*, above): no
world's base moves during a run, but an agent's forced push to the issue branch can leave a head
that does not merge, which the world still reports `CLEAN` or `BLOCKED`, and `rig.sh health`'s
rule 24 names that push. The stand-in sent
a conflicting head back to `implementing` from every phase after the implementer's, as main's
daemon did from #4291 until #4464; it no longer does. A completion's verdict is held to the
route's rule first: a tester's that is not `pass` or `fail` is refused 400
`TESTER_VERDICT_REQUIRED`, any other role's 400 `VERDICT_ROLE_FORBIDDEN`, and nothing is written.
A completion that rates the plan (`planRating` with `planReason`) is refused past
`HANDOFF_NOT_NEW`, 400 `PLAN_RATING_UNEXPECTED` in the engine's words, and leaves its fact
unspent, so the same completion without the rating is taken: only the completion that ends the
retro rates the plan, and no run stands in `retro`. A request body a string of which holds U+0000
is refused 400 before any route reads it, naming the first such string's place and where the
character stands in it in code points, in the daemon's words: main's since #4463 (`which the daemon
API refuses rather than store it as U+FFFD`) or #4488's before them (`which the daemon cannot
store`). Both refusals are main's since #4488, read off the label's source (`labelShapes`'
`refusesRating` and `nulRefusal`, which reads each wording by its own marker). #4463's store
keeps U+FFFD in the place of a U+0000 that reaches it, and no text of such a body reaches it, so
its answer differs from #4488's in the words alone; a byte that is not UTF-8 is read as U+FFFD by
every daemon and by the stand-in, and taken. Where the source holds no refusal of such a body,
main `ff2a10ef`'s, the body is taken to its route, and a
completion or a backward move the engine takes is answered 500 `FACT_APPLY_FAILED` in that route's
words, nothing written: that daemon writes a completion's summary and commit into text columns
and a move's reason into the jsonb task of the role it starts, and PostgreSQL refuses U+0000 in
both (`daemon-pane.ts`'s `STORES_NUL` reads those writes and answers by marker; the answer is
derived from that source and PostgreSQL's refusals, read on 2026-11-19, and from no booted
daemon). A ready, whose capability detail that daemon cleans of control characters, and a
request the engine refuses are taken as any other. Each move
into a phase that shows another status writes that status to the run's own Dispatch before the
request that made the move is answered, as the daemon's outbox runs a `dispatch_status` row:
under the daemon's own session, `legion-daemon:<PROJECT>` (the session the seeds write each
status as), and only while Dispatch still shows the status the move observed, so `dispatch read`
and `read_record` name one status and a status someone else gave the issue since stands. A
worker's backward move posts the daemon's sentence of the move on the issue as a message first
(`Legion moved this issue from <from> back to <to> at the <role>'s request: <reason>`), with the
outbox's marker of the row, and a move into a phase that shows the status the issue already
shows posts that sentence alone; it is posted only where the label's own workflow source holds
that sentence.
A write the run's Dispatch does not take stays owed and is tried again at the stand-in's next
request; the state's `pendingStatusWrites` lists it with why and with the sentence it posts.
Each attempt reads the issue's messages since the row was queued, as the outbox does, and posts
no sentence an earlier attempt already posted under the row's marker. A write Dispatch refuses
for good (a coded 4xx: an issue it holds none of, say) is finished refused, as the outbox
finishes it, where the stand-in owed it for ever before.

The run's Dispatch also holds the session of each live role of the issue, as the daemon
publishes it (`PUT /api/v1/issues/{key}/role-holders`, as the daemon's own session): at the
stand-in's boot, at the agent's registration, at its ready that carries no capability report,
and when a turn outcome fails its claim, each the whole set of claims registered, ready,
working or idle that record a session. So the world's Dispatch holds the tree's architect, each
role the world's history ran, each agent the run booted before this one and the agent booting
now, as production's does, where it held none before. The `dispatch` CLI reads no
role holder, so `dispatch read` shows none in production either: `GET
/api/v1/issues/{key}/role-holders` answers the set, which `rig.test.ts` holds to the record's
live claims on a world built against a real Dispatch. A label whose daemon publishes no role
holder, or whose Dispatch serves no such route (main before #4228), is published nothing; both
labels are built from main `0b2769e5` or after, whose daemon and Dispatch publish and serve it.

The run's Dispatch holds the record of each run of a step of the issue, as the label's daemon
writes it (main's since #4400, 53980e0e32): one write at the run's start, one at its end, and the
report alone of a completion that lands after that end, each `PUT
/api/v1/issues/{key}/steps/{id}` under the daemon's own session, as its outbox's executor sends a
`step_record` row (`publishStepRecord` and `stepWriteOf`; `step-records.ts` restates the writes).
The world's history is written before any agent boots (`seed.ts steps`, rig.sh's `steps_seed`):
each phase a seed recorded a handoff of, planning to reviewing, as one run of its step under its
role's claim, started where the phase before it ended and ended passed at its handoff's time,
its report that handoff and its session the one that wrote it, and naming the model the label's
profile answers with (rig.sh's `label_model`); the run of the phase the issue stands in is left
open, its start alone. The stand-in writes that open run's start again at its boot, which
Dispatch keeps as the seeds wrote it, since it writes each field of a record once; and at each
move, the end of the run it leaves (failed for a request for changes or a worker's move back,
each naming the step the work returns to, and passed for an approval), its report the
completion of the move's own role, its session the claim's and its model the last its own
agent's transcript names, and the start of the next run of the step it enters, under that step's
claim (the architect's in `accepting`). No move ends a run stopped: main's daemon since #4464
moves no issue for a head that conflicts with its base. A taken completion of a phase the issue
has left writes its record onto the ended run. Each record's id is the one the daemon derives, so
the seeds' and the stand-in's writes of one run are one record. `dispatch steps` and `GET
/api/v1/issues/{key}/steps` are the run's own Dispatch server's answer, the label's own build, so
no read of them is faked. What the stand-in and the seeds write is read off the label's source by
marker (`daemon-pane.ts`'s `labelShapes`, `writesSteps`): the outbox's `step_record` kind, the
executor's write of every field of the row through the client's route (`stepWriteOf`), the wait
`execute` makes of a Dispatch that has no route for it yet (`waitsForTheRoute`), the body that
route takes (`dispatch/types.go`'s `StepWrite`), the steps of the task and the step of each
phase (`workflow/plan.go`'s `DefaultSteps`, `stepWork` and `stepIn`, which `workflow/steps.go`'s
`stepOf` reads since #4488), the result of each move (`steps.go`'s `endOf`), the id's namespace
and name, which fields each of the three writes carries, and the route's row in Dispatch's
table. A label built before #4400 (main `ff2a10ef`, read by its outbox's kinds) is written no
step record, and its Dispatch serves no such route. Production's Dispatch, at `4055c4a5` when it
was read on 2026-11-19T09:58Z, does not serve the route yet (`404 NOT_FOUND`, `no route for GET
/api/v1/issues/INFRA-764/steps`): #4400's deploy is pending, so no production issue holds a step
record to pin a world's against.

Main's daemon since #4488 also writes the plan each run of an issue carries out (`PUT
/api/v1/issues/{key}/plans/{id}`, at the run's move out of `admitted`) and the rating the retro
gives that plan onto the retro's record (`PUT /api/v1/issues/{key}/steps/{id}/rating`). The
stand-in writes neither, and declares both not modelled: no run begins in `admitted` or stands in
`retro`, and the plan's one read, `GET /api/v1/projects/{key}/plans`, lists a project's finished
issues alone, which the world's issue never is (`class-search.json`'s unreachable entries).

What the daemon tells the roles of a head's CI is not modelled either, and the stand-in declares
it so: the CI message naming each check of the pull request's head with what it came to, the
task waking the worker of the open phase on a settlement, and what main's #4541 changed of both
(a check GitHub skipped is named `skipped`, with the sentence that it ran nothing on the head; a
settlement of a head the record does not stand at wakes nobody; a settlement the daemon drops is
named by a line of its log). No run reaches any of it: no run's Envoy publishes a checks
settlement, since `rig.sh` gives each label's listener no `ENVOY_WEBHOOKS`, so the listener
opens no CI store, whose summary loop is the one publisher of a settlement; and every check a
world's head reports is concluded `success` (`concluded_checks`), so no world holds CI running
on a head or a check GitHub skipped (`class-search.json`'s unreachable entries).

The record's shape is held to production's: `production-shapes.json`'s `legion read_record` is
the shape of one issue's record as the tool answered it in a Legion issue pod at
2026-11-17T16:49Z, the
two maps keyed by a name of the issue's (`workers`, by role, and `backwardMoves`, by pair) read
as lists, so the pin names no role and no move of that one issue. The derivation refuses a
record whose shape would carry a value as a key: every key of the daemon's record is one
camel-cased word of letters, and a path holding anything else (a map by issue key or by
session) fails it by name. `daemon-standin.test.ts` compares a running stand-in's record with
the pin (`shapeDiff`), naming every difference, and `rig.test.ts` holds the record a stand-in
serves on a world `rig.sh` built, against a real Dispatch, to that world's own facts: the ids
and versions of the seeded handoff records, the pull request as the world's `gh pr view` prints
it, the spec's approval and the status `dispatch read` shows, for a threads-closed world and a
reviewer-text-findings one. Three members production holds are in no world's
record, each for a reason of the rig's (`WORLD_NEVER_HOLDS`, `standin-record.ts`'s header):
`step`, which the daemon builds from the tool frames of the launcher's worker stream, where a
rig agent runs under no launcher; the architect's `capabilities`, since no architect's session
runs and a claim carries a report only once its session sent one on a `claims/ready` of its
own, which a `-p` run that works until it exits does not reach; and the three turn times of a
claim no agent of the run booted, which the stand-in's daemon never recorded.

Every REST route the world serves — each `GET`, `POST`, `PUT` and `PATCH` of `gh-routes.ts`'s
`WORLD_ROUTES` and each route of `github-routes.ts`'s `GITHUB_ROUTES`, the HTTP stand-in
Dispatch reads — answers every field GitHub's REST API description marks required in its
success answer, as `rest-required.json` holds them: the required and declared field tree of each route's
operation, derived by `rest-required.ts` from `descriptions/api.github.com/api.github.com.json`
of github/rest-api-description at commit `c9c95fe34ced05fe076001f5c55704742bc8e7f8` and never
written by hand. To regenerate it, fetch that file at that commit (`curl -fsSLO
https://raw.githubusercontent.com/github/rest-api-description/c9c95fe34ced05fe076001f5c55704742bc8e7f8/descriptions/api.github.com/api.github.com.json`)
and run `bun rest-required.ts api.github.com.json`, which writes the extract and formats it; a
route the description has no operation of fails it, naming the route, unless `rest-required.ts`'s
`ANSWERED_AS` names the operation GitHub answers it as: `git/refs` and `git/refs/<ref>`, whose
reads GitHub serves and the description lists none of, are held to "List matching references"
and "Get a reference", as GitHub answered them on 2026-11-21. An answer is read against
it as INFRA-764's read search reads one (`absentFields`): through `$ref`, every part of an
`allOf`, the closest of a `oneOf` or an `anyOf`, and every item of an array. `gh-standin.test.ts`
calls every REST route of `WORLD_ROUTES` in a world and holds its answer to its operation, and
fails naming a route the extract holds no operation of; `github-standin.test.ts` does the same
for the HTTP stand-in. Each REST route of `WORLD_ROUTES` (each route of `fixture_write` among
them) is held to GitHub's key set as well, a row of `gh-standin.test.ts` each (`the key set of
<route> is GitHub's`): production's pinned answer to it where it is pinned (`shapeDiff`'s missing
and extra paths), and otherwise the description's, every field required there and no field it
declares nowhere, at every level (`keyDiff`, which takes any field where an object declares
`additionalProperties` or no field at all). A row names each key missing or extra; a route with
no key set fails its row and the one that holds every route to one. A key the world answers
beyond its key set is declared with its evidence (`KEY_SET_DECLARED`): `copilot_code_review_severity`,
null, on the pull request's review comments, a review's, and the comment a create or a reply
makes, which production gives every review comment and neither the description at
`c9c95fe34ced` nor the pin of 2026-11-17 holds (`gh api repos/cybermonklabs/legion/pulls/2827/
comments --jq '.[0] | keys'`, `Date: Sat, 21 Nov 2026 10:21:12 GMT`, and `gh api -i` of
`…/pulls/2827/comments?per_page=1` and `…/pulls/2827/reviews/5816301660/comments?per_page=1`,
10:21:02 and 10:24:42 GMT, each null). Three routes are GitHub's refusal of a role App and no success, which the
world gives as GitHub's error body and gh's line: a branch's classic protection and `user` (403:
an installation token reads no administration and is no user's, measured from a Legion pod on
2026-11-17; `rig.sh health` declares the second for every role), and the jobs of a run the world
holds none of (404). The values agree across routes: a user, the repository and the pull request
are each built one way for both stand-ins (`rest-objects.ts`) from the world's accounts
(`<run>/accounts.json`: the developer, the organization the repository is in, and the bot of each
role App, so a commit a role made links to its bot), the repository's settings, the pull
request as `gh pr view` reads it and the run's remote. `GET …/pulls/<n>` is GitHub's pull request
object, not gh's view of it, opened by the author of its branch's first commit, its head the
branch's tip, its merge state `pr view`'s, its test merge (`merge_commit_sha`) the commit that
merges the head into the base, which the world writes into the remote as GitHub keeps its own. A
comment made outside a review, and a reply, is filed in a `COMMENTED` review of its own, as
GitHub files it, which the reviews list holds.

The REST reads round 25 made are pinned to production's whole shape too, and
`gh-standin.test.ts` holds each with `shapeDiff`: the repository, the pull request, its reviews
and one review, a review's comments (GitHub's older shape of a review comment, with no `line`,
`side` or start of its own), the pull request's review comments, its conversation, a commit's
check runs and combined status, a comparison, a file at a ref, the base branch's rules and the
base branch, beside the five ruleset reads above. So are the REST reads the tester made on
2026-11-18: the workflow runs, the jobs of a run the repository has not got (GitHub's 404), a
comparison as a diff, the base's ref, a directory as JSON and under the raw media type (GitHub
answers a directory as JSON either way), and the App's read of its user (GitHub's 403). Each
answers with no difference but one of state: production read its check runs while they ran, so
their `conclusion` and `completed_at` are null there and the world's, concluded, are strings; and
the review comments carry `copilot_code_review_severity`, which production has given since the pin
was read.
The run's GitHub stand-in, which the run's Dispatch reads, answers the repository and the pull
request in the same pinned shapes, and a commit by its SHA and a commit's pull requests in the
implementer's (`PINNED_HTTP`). Its fifth read, `GET /repos/<o>/<r>/installation`, Dispatch makes
with its App's JWT, which no pod holds. An installation token is refused it: `gh api
repos/cybermonklabs/legion/installation` exited 1 with `gh: A JSON web token could not be decoded
(HTTP 401)` (measured at about 08:47Z on 2026-11-18). So no pod can read production's answer, and
the installation is held to `rest-required.json` alone. A REST read the pin does not hold — the
installation, the answer of a write — is held to the fields GitHub requires and the fields its
description declares, and no more, and is not claimed to answer in production's whole shape.

The reads of the run's remote that no pin holds are held to the fields GitHub requires and
declares (`rest-required.json`) and to the answers GitHub gave on 2026-11-21, which a row of
`gh-standin.test.ts` holds for each, and to no more, and are not claimed to answer in
production's whole shape:

- `GET repos/<o>/<r>/commits/<ref>`, one commit, and `GET repos/<o>/<r>/commits`, the list;
- `GET repos/<o>/<r>/git/commits/<sha>`, a commit's git data;
- `GET repos/<o>/<r>/git/refs` and `GET repos/<o>/<r>/git/refs/<ref>`, the refs;
- `GET repos/<o>/<r>/git/trees/<sha>`, a tree;
- `GET repos/<o>/<r>/branches`, the branches, and `GET repos/<o>/<r>/tags`, the tags;
- `GET repos/<o>/<r>/pulls/<n>/commits` and `GET repos/<o>/<r>/pulls/<n>/files`, the pull
  request's commits and files.

gh's own reads of the pull request — `gh pr view`, `gh pr list` and `gh pr diff` — are the gh
stand-in's reading of the world as gh 2.98.0 prints it (`gh-standin.ts`'s `prRead`, `gh-view.ts`),
never the object `fixture_write` holds. `--json` takes the 46 fields gh takes, refuses any other
as gh does (`Unknown JSON field: "<field>"` and the fields it takes, exit 1), and serves every
one: the world's facts of its one pull request, what the run did to it (its head, checks and
merge state; its reviews, the world's, the run's and the `COMMENTED` ones GitHub files a lone
comment in, each in gh's shape with its node id, its author's GraphQL login and `commit.oid`;
`latestReviews`, the last review of each author but the pull request's own, ordered by login,
with the id and the commit gh does not read left empty; its conversation; and the files and
commits of its diff from the merge base), and the empty lists and nulls of what the world holds
none of (labels, assignees, milestone, projects, reactions, review requests, issues it closes).
A review still pending, and the thread its inline comment opens, is the App's that wrote it alone,
as GitHub shows one to its author alone: every read of the reviews — the REST list and a review
by id (GitHub's 404 for another App's, which 404 is not measured, since no pending review of
another account can be found to read), `gh pr view`, GraphQL's reviews and review threads, the
review comments — shows it to that App and to no other (`gh-standin.ts`'s `shown`), each role
calling with its App's token as production gives it (`rest-objects.ts`'s `roleApp`: the implement
App for the implementer and the merger, the review App for every other role), and each App keeps
a pending review of its own.
With no `--json`, `gh pr view` prints gh's text — thirteen `<name>:\t<value>` lines, `--` and the
body — `gh pr view --comments` the comments and the reviews gh shows, and `gh pr list` a line a
pull request; each was measured against cybermonklabs/legion on 2026-11-17. A pull request, a
branch or a repository the world has not got is refused in gh's words (`GraphQL: Could not
resolve to a PullRequest with the number of <n>. (repository.pullRequest)`, `no pull requests
found for branch "<branch>"`, `GraphQL: Could not resolve to a Repository with the name
'<owner>/<repo>'. (repository)`), exit 1, and `gh pr list` lists the pull request where its
`--head`, `--base`, `--state`, `--label`, `--assignee`, `--draft`, `--author` and `--app` match
it, as gh reads each. The record names both refusals (`calls.jsonl`'s `handler`), and `rig.sh
health` declares both for every role, since production gives them to any role that makes the
call. Every pinned read of gh's own commands is held to its shape, exit and stderr in a world
holding a review, a reply and a comment of the run's, with no difference but the states the pin
section names: the 55 reads of `gh pr view`, `gh pr list` and `gh pr diff` round 25 made, and
the 59 of `gh pr` and `gh run` read on 2026-11-18. A gh read with no pin is not claimed to answer
in production's whole shape.

`gh pr checks` prints the head's checks as gh 2.98.0 prints them to a pipe: five fields a tab
apart (the name, the bucket, the time it ran as Go writes a duration, the link, and an empty
description), failed checks first, then by name. It buckets each as gh's `aggregateChecks` does:
a success passes; a skip and a neutral conclusion skip; a failure, a time-out and an action
required fail; a cancel prints `fail` and counts as neither failed nor pending; every other state
is pending. It exits 1 once one failed, else 8 while one is pending, else 0. A head that reports
no check is refused as gh refuses it: `no checks reported on the '<branch>' branch`, nothing on
stdout, exit 1 (measured on 2026-11-18 on #4326 and #2827, `prod-reads-19.json`).

`gh auth status` prints gh's text for the pane's own gh configuration: the host, the account
`x-access-token` and its `hosts.yml`, the account active, the git protocol, and the token masked
as gh masks one (`ghs_` and a `*` for each character after it), with no scope line, since an
installation token holds no scopes (measured on 2026-11-18 from two issue pods). The world writes
the token line from the shape of the token the rig gives every pane (`daemon-pane.ts`'s
`RIG_GH_TOKEN`), so no stand-in reads a token. `-a` prints the same. Every other flag is refused,
`-t` and `--show-token` among them, and so is `gh auth token` (the residual above).

`gh run view` and `gh run watch` are gh's refusals, since the world holds no workflow run.
Naming a run, each prints `failed to get run: HTTP 404: Not Found
(https://api.github.com/repos/<owner>/<repo>/actions/runs/<id>?exclude_pull_requests=true)`;
naming none, gh's `run or job ID required when not running interactively` (`run ID required …`
for `watch`) and the command's usage. Each exits 1 with nothing on stdout (measured on
2026-11-18, `prod-reads-19.json` and the implementer's readings).

GraphQL is read as GitHub's parser reads it (`gh-routes.ts`'s `readGraphql`): a query that is no
GraphQL document is refused with that parser's message and the line and column of the token it
stopped at (`Expected NAME, actual: (none) ("") at [1, 176]`, the production reads'
`prod-gql-error.txt`, and a string no quote closes, a `{` where a `)` belongs, a `}` too many, a
missing `:`, value or argument, a misspelt `query`, a document of comments alone and a call with
no query, each measured on 2026-11-17), and a query selecting a field its type has not got with
GitHub's validator's error, its path, `extensions` and location, read against GitHub's own
schema of the world's types (`graphql-schema.json`, which `bun graphql-schema.ts
graphql-schema.json` reads with GitHub's introspection and formats; a field of a type it does not
hold is not judged). Either is answered with no data, as gh prints it: GitHub's body with no line
end, `gh: ` and each message on a line of its own, exit 1. A mutation GitHub refuses changes
nothing. An object a query names that the world has not got is GitHub's `NOT_FOUND`, with its
`locations`. GraphQL names a bot under `author` as a `Bot` by its App's bare slug, with no
`[bot]`, and under `viewer` and a thread's `resolvedBy` as a `User` with `[bot]`; REST names it
with `[bot]` too (measured on 2026-11-18 against cybermonklabs/legion's merged pull requests).
The viewer is the bot of the calling role's App: the implement App's for the implementer and the
merger, the review App's for every other role (measured that day with each App's token).

gh prints a `gh api` answer that no `--jq` reads as GitHub sent it, with no line end of its own:
a paginated list's pages as the one list they make, every other answer's pages back to back
(`{…}{…}`), and an error's body the same. What `--jq` prints, and the JSON of gh's own commands
(`gh pr view --json`, `gh pr list --json`), end with a line end. GitHub's 404 body has two forms:
for an endpoint GitHub does not serve, it is indented with `\r\n` line ends and names GitHub's
REST reference; for an object a served endpoint does not hold, it is one line naming that
endpoint's own page. Each was measured on 2026-11-18 from an issue pod with gh 2.98.0; the world
had ended every answer with a line end and indented both 404 bodies with `\n`.

A write is answered only where the world models it, each by a route of its own that names its
verb and the one path it writes, the world's repository and pull request read whole: the create
of a review, its submit and its update (`PUT …/reviews/<id>`, whose new body every later read of
that review shows, for a review the world held and one the run made alike); the pull request's
body (a PATCH sending the body alone, or `gh pr edit` with the body alone); an inline review
comment and the REST reply under one, a reply to a reply filed in its thread and replying to the
thread's first comment, as GitHub files it; a comment on the conversation (the REST POST, or `gh
pr comment`, which prints the comment's URL, its id the conversation's, from 8001); and the two
GraphQL mutations the graph holds. Every other write — another path, a verb a read's path does
not take (each read route takes GET alone), a mutation the graph holds no answer for, `gh pr
review`, `ready`, `merge`, `close`, `create`, an edit of anything but the body — is refused as
GitHub refuses an endpoint it does not serve: `gh: Not Found (HTTP 404)` on stderr, GitHub's error
body on stdout as gh prints it, exit 1, and nothing changed; `calls.jsonl` records it `unrouted`,
which `rig.sh health` names. Until INFRA-764 round 25, one route answered every POST, PATCH and PUT
no route above it took with `{id:6001}` and exit 0 and changed nothing, and a reviewer's update of
its review's body read back unchanged with no rule seeing it. Each create of a review is a review
of its own, with the next id (6001, then 6002), at every read. A write whose path carries a query
string (`…/reviews?x=1`, `…/reviews/<id>/events?x=1`, `…/pulls/<n>?x=1`) is the write its path
names, since GitHub reads a write's parameters from its JSON body. A flag's value is read as gh's
flag parser (spf13/pflag) reads it: `--body=` carries nothing and `-b=` carries `=`, and `gh pr
edit --body ""` leaves an empty body. An empty body is `null` through REST and `""` through
GraphQL and gh (measured on 2026-11-18 on cybermonklabs/legion #612). A mutation sent to `gh api
/graphql` or `gh api 'graphql?…'` is answered as GitHub's GraphQL endpoint answers it: gh posts
each field as a key of the body, so the endpoint reads no variable from it, and gh prints
GitHub's answer, errors and all, with exit 0 (measured on 2026-11-18). The daemon stand-in's
writes follow the daemon the same way: a backward move the workflow has no row for, or by a role
not running the
issue's phase, is refused `BACKWARD_REFUSED` in the engine's sentence; one whose sentence, the
message the move posts on the issue, Dispatch would refuse is refused before anything moves,
`BACKWARD_REASON_DECISION_BLOCK` for a decision block in it and `BACKWARD_REASON_TOO_LONG` for one
past Dispatch's bounds, in the engine's sentence around Dispatch's own, as the label's daemon
reads it: the longest post its outbox could make of the sentence, read by the label's own
Dispatch reader through the label's build of `daemon-pane.go` (`post-check`), and only where the
label's workflow source holds those refusals; and one it takes moves the issue and posts its
sentence (above). A request of a route the label's daemon does not register is answered as its
catch-all answers it, 404 `{"error":"no route"}`. The stand-in serves only the routes an agent's
plugin calls: the daemon's `GET /healthz`, `GET /readyz` and `GET /legion/v1/operator/claims`,
which no plugin calls (`packages/pi-legion/src`, `packages/pi-shared/src` and
`packages/claude-envoy/src` name none of them), are answered 404 too, as the daemon's catch-all
answers a route it does not register; the label's daemon registers them, so `rig.sh health`'s
routes rule makes a run that asked one unhealthy. A completion naming a handoff record reads it
back from the run's own Dispatch and is
refused `HANDOFF_RECORD_NOT_FOUND` or `HANDOFF_RECORD_MISMATCH` as the daemon refuses it, and
`HANDOFF_SESSION_MISMATCH` where another session than the claim's wrote it (measured on round
25's 40 worker runs: each of the 31 records an agent wrote names the session that agent
registered, so the check holds a record to the right session; a record a seed wrote for the
role an agent plays is the seed's session's, and the agent is told to write its own). A
completion is then one fact, by its role, the phase the issue stands in, the pass, the record or
commit it reports and its verdict: a call that repeats one the workflow was handed, the
completions the seeds stand for among them, is answered `HANDOFF_ALREADY_RECORDED`; a late
completion of a role whose row holds one, seeded or taken, `HANDOFF_NOT_CURRENT_PHASE`; and
one naming the record the role's row last named `HANDOFF_NOT_NEW`, each in the daemon's own
sentence and only where the label's own daemon source holds that code. So a threads-closed
reviewer's late completion is refused, its seeded review round standing on its row, where
the stand-in took it until it read the seeded rows (3 of round 25's runs).
`completion-answers.ts`'s `ANSWERS` allows those answers accordingly.

Each role commits as its App's bot, `<slug>[bot]` and
`<id>+<slug>[bot]@users.noreply.github.com`, as production gives it (`packages/daemon/internal/appauth`'s
`AppRoleFor` and `botIdentity`): the implement App for the implementer and the merger, the review
App for every other role (`rig.sh`'s `role_identity`). A pane gets the six variables production's
runtime sets (`role_env`), and adopts the shared workspace's working copy at its boot as production
does (`adopt_workspace`); the implementer's seeded commit is the implement App's. The default
branch's own history, and the brainstorm run's repository, are one developer's
(`world_author_name`), which no role of Legion wrote.

Stated limits, each known and kept so:

- A just-pushed head's checks report `completed` at once, where GitHub would report them in
  progress for as long as CI runs. Modelling that window would make the world's timing something
  an agent waits on while the daemon stand-in sends no wake when CI settles, which is behaviour
  production does not have here, so the window stays out.
- The check runs name GitHub Actions as their App, while the world holds no workflow run
  (`actions/runs` lists none, and `gh run view` refuses as for a run that is not there).
- The REST reply route (`POST repos/<o>/<r>/pulls/<n>/comments/<id>/replies`) follows GitHub as
  measured on 2026-11-17 (a reply to a reply is filed in its thread, replying to the thread's
  first comment); the `in_reply_to` of `POST …/pulls/<n>/comments` is read the same way, which is
  not measured.
- `mergeWait.checkRuns` is 0, the reading the contract gives no evidence by: it counts the runs
  a CI settlement brought the record, and no settlement reaches a stand-in. The check runs a head
  reports are the gh stand-in's.
- The record holds the fields production's did at the read the pin was made from, served to a
  plugin at contract 144. The two times of the architect's entry handshake and the architect's
  stalls, which a daemon at contract 146 writes for a root, and an issue's experiment
  assignments, which that read held none of, are not served.
- An endpoint written with gh's `{owner}/{repo}` placeholders is not filled in from `GH_REPO` as gh
  fills it, so such a write is refused as one the world does not model; no recorded run made one.
- A GET of a repository the world's workflow names (an action's tag, read to check the commit its
  `uses:` line pins) reaches the live GitHub, as it does in production, so its answer is GitHub's
  and not the world's. A run in which that read fails for a network reason is not unhealthy for
  it, since a production reviewer can meet the same failure.
- The readers resolve literal text flow and nothing else: the words of a `for` loop's literal list,
  a literal assignment, and the four operators that only take text off, `%`, `%%`, `#` and `##`,
  over a variable that holds a literal. A variable whose value needs a command's output, a file's
  contents or arithmetic stays unresolved, and a call that reaches GitHub through it is off the
  world. A recorded run that needs a shape outside that gets no special case: its count is
  reported, and that count is the evidence that buys running the label's own `legion gh` wrapper
  in front of the world's `gh` (measured at about 5.6 minutes of build a label).
- A behaviour of the Legion gh wrapper that its selector table does not describe is not modelled:
  a gh alias or an extension, a set `GH_TOKEN`, `GITHUB_TOKEN` or `GH_HOST`, an option or an
  argument the wrapper cannot place, the working directory's git remotes (a world's workspace's
  origin names the world's repository, and the world's gh reads the repository from the pane's
  `GH_REPO`, never from a remote; a call for which the wrapper would read them is read as naming
  the world's owner, so the world runs none of them on the pane's token), and an option gh's help
  gives a command that the label's saved help does not show (`gh variable list -R`: no saved help
  shows that group's inherited `-R`). The world then answers such a call otherwise than
  production, health calls the run unhealthy, and the run is replaced.
- Nor are the wrapper's refusals of a command, an option or a working directory whole, which it
  holds outside its selector table (`gh.go`'s `ghRefusedCommands` and `ghRefusedOptions`,
  `gh_gitconfig.go`'s `ghRepoGitConfigRefusal`, main's since #4424, fdce4e94d2): `gh agent-task`,
  `gh auth login`, `logout`, `refresh`, `switch` and `setup-git`, `gh config set` and `gh gist
  edit`; `--editor`, `--attach` and `--recurse-submodules`; a git flag `repo clone`, `repo fork`
  or `gist clone` passes on that is not on its short list; and a repository configuration naming
  an `http` ssl or proxy key, another credential helper, an include or `extensions.worktreeConfig`.
  The world's issue workspace holds none of those keys (its helpers are `!legion git-credential`
  and the empty one, which the wrapper takes), so that refusal never applies in it. A command the
  world routes none of is refused as an unmodelled call (`gh: Not Found (HTTP 404)`, recorded
  `unrouted`), which health reads as a refusal the scenario does not declare; a call carrying a
  refused option that the world routes is answered by that route, where production's wrapper
  refuses it.
- Of gh's reads of the pull request, `gh pr list --search` is refused as a call the world does not
  model (`gh: Not Found (HTTP 404)`, recorded `unrouted`), and `--template`, `--web`, and `gh pr
  diff`'s `--patch`, `--exclude` and `--color always` are not read: such a call is answered as if
  it did not carry them.
- GraphQL the world does not read — a fragment spread, a fragment definition, a directive, a
  document of two operations — is refused as a call the world does not model, where GitHub runs
  it. A field the vendored schema holds no type of is not judged, and GitHub's other validator
  refusals (a selection on a scalar, a variable declared and not used) are not modelled.
- Off a deployment's pod, the agents' git reads that host's system configuration, which the world
  shows as the host holds it: a developer machine's or a runner's file, not production's (a GitHub
  runner's holds `safe.directory=*` and four `filter.lfs` keys). On a deployment's pod it is that
  deployment's own; a run off the pod has no other to show, and its `services-from` records which
  file it was by digest.
- The issue workspace's clone holds one key no production clone does, `remote.origin.vcs=ghapp`,
  and the pane's PATH a helper no pod's does, `<run>/bin/git-remote-ghapp`: the transport that takes
  origin's fetches and pushes to the run's remote. It takes a call of origin by its name alone; a
  call naming `https://github.com/<owner>/<repo>` reaches the live github.com, which health calls
  off the world. The helper names the App the remote reads a push as (`GITHUB_APP`), where a pod's
  push carries it in its token, and a push that names `<run>/remote.git` itself, which no pod can
  make, carries none, and the remote refuses it no workflow change.
- The pane's `PUBLIC_LEAK_CHECK` names the world's run of the shared check,
  `<run>/lib/public-leak-check.sh`, where a pod's names the check itself: the run that gives the
  check's own git the world's exec path (`<run>/lib/git-core`), so its `ls-remote` of origin's URL
  reads the world's heads.

### The class search

`rig.sh class-search [<checkout>]` runs every class of defect INFRA-764's review rounds found as a
named check of a checkout, and judges each hit by the rule the check states (the architect's
ruling of 2026-11-18T08:40Z). With no `<checkout>` it searches the checkout `rig.sh` lies in; with
one it runs that checkout's own `class-search.ts` over that checkout's tree, so the search of a
commit is that commit's own command. It needs bun, git, jq, ssh-keygen and go (each world gets the
checkout's gh selectors, read by `gh-selectors.go` as `worker_boot` reads them), and takes about
three minutes on a pod of two CPUs. The children it starts, each world's build and each gh call,
carry the caller's `PUBLIC_LEAK_CHECK` as a pane does (`leak-check-env.ts`), so on a pod, whose
`git` is Legion's leak-check wrapper, a world's pushes find the shared check through it.

It opens with the free space of the checkout's file system and of `$TMPDIR`'s, as statfs reads
each at the search's start and at its end. For each check it prints the class, the reading it
makes, its rule, the count of its hits, and each hit: `<file>:<line>` (or the world, route or read
it names), its text cut, and its verdict, `INSTANCE` or `true` with the reason. Then it prints the
findings outside the residuals, the residuals found with each one's ruling, the residuals no hit
matches any more (stale), the declared-unreachable entries found with each one's reason, those no
declaration matches any more
(stale), and the 46 entries of the hand search of `f853f11b`. It exits 0 when no check has an
instance that neither a residual nor an unreachable entry covers and no entry of either is stale,
1 when one has or one is, 2 when a check cannot run (stderr names it, with the two disk readings),
and 3 when its report or its error line cannot be written whole: each is written with
synchronous writes, each checked, and a line naming the write that failed, with the search's own
status, goes to the other stream. Where bun exits nonzero, `rig.sh` adds `class-search: bun
exited <n>` on stderr and exits with bun's status.

The checks:

- Writes. Every world `rig.sh`'s `worker_fixture` builds (each case of each worker scenario of
  `run-names.ts`'s `SCENARIOS`) is built with the script's own functions; only `fixture_workspace`,
  which writes no route, is left out. Each route of its `fixtures.json` that takes a write verb or
  names none gets each write that reaches it, and each write command of gh v2.98.0
  (`gh-writes.json`) is called, through the gh stand-in in a copy of the world. A refusal is no
  finding. A success is modelled only where a read of the world (`gh pr view`, the reviews, the
  review comments, the conversation, the review threads) answers otherwise after it. No route may
  answer a call to a path where the world holds nothing. Each App's push permission follows
  `appauth/permissions.go`'s `writePermissions`: in a copy of each world, the role each App serves
  (the implementer, the reviewer) pushes a commit that changes `.github/workflows/greet.yml` over
  the pull request's branch through the remote helper `rig.sh` writes for it (`world_remote_app`),
  and an App holding `workflows: write` must be taken and one without it refused with GitHub's
  line.
- Reads. Each GET the world serves (the gh world's `WORLD_ROUTES`, the GitHub stand-in's
  `GITHUB_ROUTES`, the daemon stand-in's) must be asked by a pinned read of
  `production-shapes.json`, which `gh-standin.test.ts`'s coverage row holds to a row, or be named
  in this README from *Which of the world's answers are pinned* on. Each pinned gh read is made in
  the approved world after a comment, a review and a reply: an HTTP refusal production gave that
  the world does not give is an instance, and so is a field of production's answer that the
  world's answer lacks. The check also finds the remote helper key on the world's clone and the
  `gh auth` calls that print the token.
- Readers. The code `health.ts`, `score.ts` and `judge.ts` can run, with the modules they import,
  is read for TMPDIR, HOME, the working directory, LEGION_WORKSPACE, RIG_ROOT, the work directory,
  `rig.sh world`, a profile or an install, and the reader's own checkout. A content-keyed cache, a
  scratch directory of the reader's own, a program of the rig's own and a path under the run's
  directory are no finding. Each text test of `score.ts` and `health.ts` against a world id is
  listed, and one that tests a call's own text for what the world did with the call is an
  instance.
- An agent's world. A record that `rig.sh` copies into the top of a run's directory, whose writer
  prints the rig's own directory, is an instance. So is each `mount --bind` of `rig.sh` whose
  source is content the rig made, bound over a path of the agent's world: the agent reads there
  what the rig declared, where production's pane reads the host's own file, as the empty file once
  bound over the system git configuration on the premise that the worker image had none. A bind
  into the holder (a tmpfs of its own at `/dev/shm/holder`, inside the run's own `/dev/shm`), a
  held path bound back at its own path, and the run's own
  `<run>/tmp` bound onto `/tmp` are no finding. A file `rig.sh` builds into every run
  (`--outfile "$R/<path>"`, the `gh` bundle), whose words no reading of `rig.sh`'s text sees, is
  held by `rig.test.ts`'s row that builds and reads it (the launch guard's section above), and is
  an instance where that row is not there.
- A stand-in's declaration of a behaviour it does not model. Every program of the rig (each
  stand-in it runs among them: `daemon-standin.ts`, `standin-record.ts`, `gh-standin.ts`,
  `github-standin.ts`, `daemon-pane.ts` and `daemon-pane.go`) is read for `not modelled` in a
  comment, its words across the comment's lines read as one, with the sentence that holds it and
  the `Unreachable:` sentence after it, which gives why no scenario's world reaches the behaviour
  (the architect's ruling of 2026-11-19T04:50Z). Each declaration is an instance, which a
  residual covers where one was ruled, and an unreachable entry where the worlds show no
  scenario reaches it; a declaration neither covers is a finding at its file and line.
- A test file whose rows run processes under bun's 5 s default bound. Every test file of the rig
  is read for a code line that calls `spawnChecked`, `Bun.spawn`, `Bun.spawnSync`, `spawnSync`,
  `execSync` or `Bun.$` outside a string, a helper's of the file among them. Such a file is an
  instance unless its top level calls `setDefaultTimeout(<n>)` with `n` a number of at least
  30 000; a file that runs none is a true statement. A process run by a module the file imports
  is not read.
- The hand search's greps, each with the rule its hits are judged by.

`class-search.json` holds the residuals, the declared-unreachable entries and the map of the hand
search. Each residual names the check that finds it, the pattern of the hit it covers, the date of
the architect's ruling, and this README's words for it. Each unreachable entry names the check,
the pattern of the declaration it covers, which holds the declaration's `Unreachable:` sentence,
and the reason, read from what the worlds serve: a run's boots, the `legion` tool, the record's
moves, the gh stand-in's head and a run's project. Each of the hand search's 46 entries names the
checks that run it, the rows that hold it, or why no mechanical search can.

What it does not check:

- An entry of the hand search marked `notMechanical` in `class-search.json`, each with its reason
  (a merge's history, a sentence's meaning, two programs read side by side, a mutant, CI), and the
  part of an entry left to the rows its `held` names.
- A write's effect that none of the five reads shows. Each route gets one write a verb, with a body
  alone and with a review's fields. A route met in several worlds is written in the first, and a
  gh write command no route serves is called in one world only.
- A pinned answer's kinds and line shapes, which the rows of `gh-standin.test.ts` hold. The search
  reports only an HTTP refusal and a missing field, and fills a pinned read's placeholders with
  the world's values.
- A reader's value under another name. The readers are read by their text: a definition is
  reached where a reached part names it, so a name in a string reaches it too, and a value passed
  under another name is not followed. `rig.test.ts`'s row that judges one archived run under two
  environments is what runs the readers.
- A record written straight into a run's directory. Only a `cp <file> "$R/<name>"` is read.
- Content the rig gives a path of the agent's world by other means than a bind (a variable such as
  `GIT_CONFIG_SYSTEM`, a file written over the host's). Only `mount --bind` is read.
- A text test whose pattern or subject is built far off. A pattern is read off the line or a
  top-level const of its file, and a subject off the line or one assignment in the same
  definition.
