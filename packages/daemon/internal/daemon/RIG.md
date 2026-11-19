# The rig of this change's checks

The live proofs w1 to w5 of this change's verification run on one rig: the whole daemon, as `localruntime_live_test.go` boots it, among the real things production runs around it.
This note is how a proof uses it.
`rig_live_test.go`, `rig_nats_live_test.go`, `rig_runtime_live_test.go` and `rig_sql_sessions_live_test.go` are the rig, each file's header says what it holds, and `lockout_live_test.go` (w3) is the first proof on it.
Every file is behind the `e2e` build tag.

## What the rig is

| Part | What runs | Where it is |
| --- | --- | --- |
| NATS | the real `nats-server` that `scripts/e2e/lib/live-daemon-proof.sh` installs (v2.15.1, its `nats_version`; the daemon refuses a server below v2.11.12, v2.12.4 or v2.14.0 at boot), JetStream on, `/connz` on | `rigNATS` |
| Grants | a client that presents no credential is `members`, the `no_auth_user`, holding `core.SessionPermissions`; the listener holds `core.ListenerHostPermissions(legion-rig)`, Dispatch `core.DispatchPermissions(legion-rig)`, all three read at the listener's commit; the daemon holds `natsgrant.Daemon` of this head | `rigGrants`, `scripts/e2e/lib/live-rig-grants.go` |
| Envoy | the listener production pins, `testenvoy.PinnedRevision`, built from that commit as its image is built, with a session age of 5 minutes; with `LEGION_E2E_LISTENER_FROM_CHECKOUT=1`, the listener of the checkout's own commit, built the same way (below) | `testenvoy.RigBinary`, `r.listener` |
| The listener's front | a reverse proxy every pane and the daemon reach the listener through | `r.front` |
| The daemon | `run`, over a PostgreSQL database of the run's own, its own NATS user, and the rig's runtime | `r.boot`, `r.d` |
| Workers | the real `legion worker-shim` around a real Oh My Pi (`LEGION_OMP_PATH`) with this head's two plugins, each a child process group | `r.runtime` |
| The worker stream | a relay that carries every shim's connection to the daemon's socket | `r.relay` |
| The model | the gateway the run names, or a scripted stand-in | `rigOptions.standin`, `r.standin` |
| Dispatch | the stand-in the other proofs use, and optionally a real scratch Dispatch that publishes on the rig's NATS as its own user | `rigOptions.dispatch`, `r.scratch` |

## Running a proof

A proof has an entry script under `scripts/e2e/` that sources `lib/live-rig.sh` and calls `live_rig_proof LABEL SLUG '^TestName$' "$EVIDENCE"`.
That run checks its inputs, builds the pinned listener (or, with `LEGION_E2E_LISTENER_FROM_CHECKOUT=1`, the checkout's) in a detached worktree and reads its grants, builds the driver, and runs the one test.
`scripts/e2e/lockout-generations.sh` is the example to copy, and its `--base-commit` is the pattern for a negative control: it lays the rig and the driver over a worktree of the base and runs the named steps there with `live_rig_proof … fail`.
`scripts/e2e/live-rig-smoke.sh` (`rig_smoke_live_test.go`) is the rig's own proof: it runs each seam below that w3 does not, the stand-in model and the scratch Dispatch among them.
A proof that adds a part to the rig adds its step there.
A negative control builds the rig from the files its `rig_files` list names, laid over the base, so a new file the rig needs goes on that list in every control that runs it.

The inputs are these:

- `LEGION_TEST_PG_DSN`, on a loopback host.
  The run makes a database of its own on that server.
- `LEGION_OMP_PATH`, an executable Oh My Pi.
- The model, as a base URL and a key:
  - `LEGION_E2E_MODEL_BASE_URL`, or `ANTHROPIC_BASE_URL` when that variable is unset;
  - `LEGION_E2E_MODEL_KEY_COMMAND`, an Oh My Pi `!command` that prints the key, or `LEGION_E2E_MODEL_KEY_FILE`, a file that holds it;
  - `LEGION_E2E_MODEL` names the model, and `claude-sonnet-5-5` is used when it is unset.
  On a development host, the base URL is `https://model-gateway.<tailnet>.ts.net` and the key command is a command that prints the gateway key.
- `LEGION_E2E_NATS_BIN`, optional: without it, the run installs the pinned `nats-server`.
- `XDG_CACHE_HOME` on the same filesystem as the driver's temporary directory, or `testbin.OMPHome` cannot link Oh My Pi's natives (`EXDEV`).
- `LEGION_E2E_LISTENER_FROM_CHECKOUT`, optional, `1` or unset: with `1` the run builds the listener and reads its grants from a detached worktree of the checkout's own HEAD, whose `packages/envoy` and `packages/contracts` must match it, in place of production's pin (check `checkout-listener` in place of `pinned-listener`), and the driver takes a listener of that commit and no other (`testenvoy.RigBinary`, `LEGION_E2E_LISTENER_REVISION`).
  Only w2 (`scripts/e2e/cut-turn-note.sh`) sets it: it tests this change's plugins and listener together, and the pinned listener predates the one that lets a controller register its notice lane.
  w1, w3, w4 and w5 run the pinned listener, and the rig takes no other build while it is unset.
  Every run writes `listener.txt` to its evidence, naming the listener's source and commit, and the driver's header line names them too.

## Starting the rig, seeding it, booting the daemon

```go
r := startRig(t, rigOptions{script: "scripts/e2e/<proof>.sh"})
r.seed(rigSeed{issue: r.cfg.Project + "-1", phase: phase.Implementing, role: claim.RoleImplementer,
	task: "Reply with the one word READY and nothing else. Call no tool."})
r.boot()
r.registerController() // only where a step reads the controller's notices
token := r.token(r.cfg.Project+"-1", claim.RoleImplementer)
r.awaitReady(token)
```

`startRig` skips the test when an input is missing, naming it.
`seed` records each issue as its phase's start leaves it: the claim is mid-launch, and its task is pending on the claim.
`boot` starts the daemon, which launches each claim's real pane.
The steps run through `r.step(name, body)`, and `r.fails(assertion, …)` names the assertion that a negative control reads back (`FAIL <step> at <assertion>`).
`r.liveAgentProof` gives the reads every agent proof shares: `claim`, `awaitReady`, `await`, `awaitLogFrom`, `logs`, `legionClaims`, and the daemon's own log as records (`r.daemon`).

## A session and a role

A Legion session is a seeded claim's pane.
Its plugin registers with the real listener through `r.front`, claims its role topic, and connects to NATS with no credential, as `members`.
`r.claim(token).Session` is its session id.
`r.nats.connections()` is `/connz`, and `r.nats.userOf` names the user that each connection came in as.
`GET <r.listener.BaseURL()>/v1/sessions?include=unresponsive`, with `r.listener.Token()` as the bearer, is Envoy's own session list.

A session that holds no claim is `r.runtime.startSession(name, r.cfg.EnvoyTokenFile, env, args...)` (w2's last step): `omp --mode rpc` in the rig's profile (`r.home`, `r.profile`), with `localRuntime.paneEnvironment` and no `LEGION_` variable, the overlays every pane is told, `ENVOY_URL=r.front.url`, `ENVOY_TOKEN_FILE` and `ENVOY_NATS_URL=r.nats.url`, then `env`.
No shim and no daemon stand around it: a step speaks its RPC (`send`, `answer`), and `--resume=<session file>` among `args` resumes it.

The controller's notices are read where the stream stored them: `r.controllerNotices(kind)`.
It reads `notify.ControllerTopic(project)` through the harness's own NATS user.
No controller process runs; `registerController` registers the operator's controller with the daemon, as `legion controller start` does.
A check that runs the project's controller as a daemon claim sets `r.cfg.ControllerLaunch = config.ControllerLaunchDaemon` before a boot, and calls no `registerController`: the daemon's keeper launches a real controller pane through the rig's runtime and sends it its start message at each ready (w2's third step restarts the daemon so).

## The seams

| Seam | Call |
| --- | --- |
| Stop a session's registrations at the listener (w5) | `r.front.blockRegistrations(session)`; `unblock`; `refusals(session)` |
| The listener does not answer (w4) | `r.front.hold()`; `r.front.answer()` |
| The listener accepts a session's next registration and does not answer it (w4) | `r.front.hangNext(session, d)` |
| Every registration and role claim of a session as it reached the front, before any hold, with when it ended and how it was answered (w4) | `r.front.arrivals(session)` |
| The listener gone | `r.listener.Stop(t)` |
| The listener started again on its port each time it exits, as its Kubernetes Deployment does (w4's NATS outage: the pinned listener ends itself after 10 minutes without NATS) | `r.listener.RestartOnExit(delay)`; `r.listener.Restarts()` |
| NATS drops every connection (w4) | `r.nats.stop()`, then `r.nats.start()`, on the same ports and store; the NATS ports, the listener's and the daemon's API port are each reserved for the whole run (`testenvoy.ReservePort`), so a restart finds its port free |
| A stored message | `r.nats.storedOn(subject)`; a wildcard subject reads every subject it matches |
| The daemon stops for an outage (w1) | `r.d.stop()`; `r.holdPortAgain()` holds the port open and nobody answers on it; `r.boot()` starts the daemon again on the same port |
| Restart the daemon | `r.restartDaemon()` |
| A process reported gone while it runs (w3) | `r.runtime.detach(token)`; once a pane's process group ends, the rig ends what that generation left under the claim's state home (`endRoleProcesses`), as its PID namespace's end does in a pod, so the next generation the daemon started beside it keeps running |
| A shim of a given boot token, or of none the daemon minted | `r.runtime.startShim(name, bootToken, argv...)`; `r.runtime.current(token).spec.BootToken` is a launch's token |
| Every read of a process by the daemon | `r.runtime.probesOf(token, from)` |
| Each launch's environment | `r.runtime.extraEnv = func(runtime.SpawnSpec) []string { … }`, set before `r.boot()` |
| SQL sessions (w2): every worker keeps its session as rows of a database of the run's own on `LEGION_TEST_PG_DSN`'s server, and every resume reads it from its row | `startRigSQLSessions(t, r)` before `r.boot()`, which sets `extraEnv` and `r.runtime.sessionKept`; `read(path)` is a session's text (`rig_sql_sessions_live_test.go`) |
| Each launch's `legion worker-shim` flags (w4's agent-secrets enrollment) | `r.runtime.extraArgs = func(runtime.SpawnSpec) []string { … }`, set before `r.boot()` |
| The plugin's heartbeat stops (w4) | through `extraEnv`: `ENVOY_HEARTBEAT_STOP_AFTER_MS`, and `ENVOY_REGISTRATION_WATCHDOG_MS` to shorten R4's own restart of it (`packages/pi-envoy/extensions/envoy.ts`) |
| A shim that reads the stop frame and does not stop | `r.relay.makeStubborn(bootToken)` |
| Close a connection from the daemon's side | `r.relay.cut(bootToken)` |
| Keep a shim's redial from the daemon | `r.relay.hold(bootToken)`; `release` |
| A daemon that answers late: its prompts to a claim's shims, this launch's and its next ones', and every line behind each, wait at the relay (w2) | `r.relay.holdPrompts(token)`; `releasePrompts(token)` |
| The frames each side sent, and the times | `r.relay.connsOf(bootToken)` |
| A request over the worker stream | `conn, _ := r.conn(token); conn.GetState(ctx)` |

## A stand-in model and a stand-in broker

`rigOptions{standin: true}` routes every pane's turns to `r.standin`, a `paneStandin` (`claim_compaction_live_test.go`), which answers every turn by default and can be set to `holdTurn`, `errorTurn`, `failTurn` or `releaseTurn`.
A proof whose claims must each do something different scripts its turns in front of that handler, as `busy_relaunch_live_test.go`'s `scriptedStandin` does.
w5 runs this way.

The rig's stand-in secrets broker is w4's own, `w4Broker` in `w4_broker_live_test.go`: it holds the first renewal unanswered until the renewer gives up on it and answers every later one with a lease.
The shim gets the real `agent-secrets` built from the checkout under test, the stand-in's address and a key directory through `extraEnv`, and the three flags the Kubernetes runtime's launcher passes through `extraArgs`; the rig hands it its enrollment as the Kubernetes runtime's enroller does, with `conn.AgentSecretsEnrollment(ctx, id)` on the daemon's own connection (`r.conn(token)`).
`packages/daemon/internal/shim/agentsecrets_renewal_test.go`'s `silentBroker` is the unit test's broker, which answers nothing.

## A scratch Dispatch

`rigOptions{dispatch: true}` starts `r.scratch` (`startScratchDispatchOn`).
It is a real Dispatch on a database of its own, and it publishes its events on the rig's NATS as Dispatch's user.
The daemon still reads the stand-in until the proof sets `r.cfg.DispatchURL` and `r.cfg.DispatchTokenFile` to it, before `r.boot()`.
Point the daemon at it only with the project and the repository mapping that the daemon's boot reads already in place.
