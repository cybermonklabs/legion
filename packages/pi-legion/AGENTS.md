# Pi Legion

`@sjawhar/pi-legion` is the Oh My Pi plugin a Legion pane loads beside `@sjawhar/pi-envoy`: the Legion entry (`extensions/legion.ts`), its lifecycle modules (`src/`), the task agents Legion's prompts dispatch (`agents/`), and the Legion skills that `scripts/pi-plugin-prepack.sh` at the repository root stages into `dist/skills`.
The entry is inert without `LEGION_TREE`, `LEGION_ROLE` or `LEGION_CONTROLLER` in the environment: a person's own session gets nothing from it.
The daemon client, the claim boot, the `legion` tool's phase ops and the phase reminder live in `@legion/pi-shared` (`packages/pi-shared/AGENTS.md`), shared with claude-envoy.

## Commands

- From this directory: `bun run test` (one file: `bun test src/tools.test.ts`), `bunx tsc --noEmit` and `bunx biome check extensions/ src/`.

- The real-binary tests: `LEGION_TEST_OMP=<omp> LEGION_TEST_NATS_URL=<nats> bun test ./extensions/*-omp.test.ts`.
  Each pane registers against a real NATS server, since a session with no relay subject gets no consumer and its claim boot ends it.

## What this plugin needs from pi-envoy

- The entry claims roles, matches injected user turns and records its bootstrapped session through the interface pi-envoy publishes (`@legion/pi-shared/interface`).
  In a Legion session it refuses to run without that interface at its own version, or beside the pre-split package (`envoyPluginRefusal()`), and exits the process; the daemon counts that as a launch failure.

## The daemon contract

- `package.json`'s `legion.daemonApiVersion` is the contract this build speaks; it equals `DaemonAPIVersion` in `packages/daemon/internal/api/version.go`, whose doc comment holds the bump rule, the files that carry the number, and each contract's history.

- A change to a shape the daemon client parses, or to a variable a pane reads, bumps both in the same commit.
  The daemon's boot gate refuses a plugin whose number differs, and the daemon relaunches a session registered on another number onto the current plugin.

## Rules

- Build every tool schema field through the injected `pi.zod` (`toolSchema` in `src/tools.ts`): Oh My Pi's converter reads internals only its own Zod produces, and one field from another Zod fails the whole extension load.

- A daemon 4xx on `claims/register` exits the process (`exitProcess(1)` in `src/claim-session.ts`): none changes on retry, and a process left up unregistered would only wait out the daemon's registration deadline.
  Two of those 409s cost the claim nothing (dispatch://INFRA-4108): the one a process an earlier release started gets for a claim that records a session and no session file, and the one a start that had Oh My Pi resume the claim's session by its id gets when it comes back as another session.

- A `task` subagent shares its parent's identity (`subagentSessionCheck` in `@legion/pi-shared/subagent-session`): it claims no role, calls no daemon route, gets no `legion` tool and never exits.

- The claim takes the Envoy role last, at the session's first turn or after `LEGION_CLAIM_FIRST_TURN_BOUND_MS` (30 s): the listener hands over every message kept for the role the moment it is claimed, and those messages must arrive inside a turn.
  `session_start` still awaits the rest of the boot, and Oh My Pi runs no RPC command that starts a turn until every `session_start` handler has returned, so a boot that waited there for a turn would deadlock.

- `src/capability-report.ts` reaches Oh My Pi's own modules only through string-literal `import()` (declared in `packages/pi-shared/src/omp-host.d.ts`): they exist only inside Oh My Pi's bundle.

- Do not alter `~/.omp` from this package; the README has the development install.

## The controller

- A daemon that sets `controller: daemon` launches the controller in a pod of its own, and the controller's whole claim runs at `session_start` (`src/controller-session.ts`).
  Otherwise an operator starts one with `legion controller start`.

- The operator's controller claims nothing at `session_start`.
  Oh My Pi holds every send a `session_start` handler makes until its startup has subscribed to the agent, so a role claimed there lets an Envoy card start a turn ahead of the start message.
  It claims from `/legion-controller-start`, the word `legion controller start` puts on Oh My Pi's command line, and sends the start message right before the Envoy role claim (`extensions/controller-start-omp.test.ts`).

## The phase stall

- A phase worker's phase opens on the daemon's assignment and closes only when the `legion` tool's `handoff_complete` or backward move succeeds; the Oh My Pi hooks in `extensions/legion.ts` drive the shared machine.

- An Envoy delivery is not an assignment.
  The entry asks the interface's injected-user-turn record at `message_start` (`matchInjectedUserTurn`) and counts every turn the record misses as an assignment, which opens even a closed phase:
  a typed turn the host starts under a new session id, and, on a host without `sendUserInput`, a turn sent in after the run's last queue poll and before its `agent_end`, which the dashboard then shows twice.

- A turn the provider refused earns no continuation.
  `src/turn-outcome.ts` reports each turn's outcome to `POST /legion/v1/claims/turn-outcome`, and the daemon fails a claim whose run of refusals reaches `launch_failure_limit`.

## Tests

- `extensions/legion.test.ts` starts each test from no `LEGION_*`, `ENVOY_*` or `DISPATCH_*` variable, points `ENVOY_CONFIG_FILE` at an empty envoy.json, and resets the shared interface after each test (`resetEnvoyPluginInterfaceForTests`).
  A new test sets only the variables it declares and stubs `fetch` itself.

- `src/daemon-api-version.test.ts` holds `legion.daemonApiVersion` to `packages/contracts/fixtures/daemon-api/version.json`, which the daemon's golden test writes, and `src/skills-guard.test.ts` holds the staged skills to `@legion/pi-shared/test/skills-guard`'s rules.
