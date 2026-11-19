# Pi Envoy

`@sjawhar/pi-envoy` is the Oh My Pi plugin every session loads: the Envoy tools, delivery of the session's messages from the Envoy listener, the `dispatch` command, the context it adds to model requests, and AskGate.
Its one entry is `extensions/envoy.ts`; `scripts/pi-plugin-prepack.sh` at the repository root stages its skills into `dist/skills`.
A Legion pane also loads `@sjawhar/pi-legion`, which reaches this plugin only through the interface in `@legion/pi-shared` (`packages/pi-shared/AGENTS.md`).
`README.md` describes the behaviour; "Session consumers" in `packages/envoy/AGENTS.md` states the listener's side.

## Commands

- From this directory: `bun run test` (one test: `bun test extensions/envoy.test.ts -t "<name>"`), `bunx tsc --noEmit`, `bun run lint` and `bun run build`.

- The real-binary tests: `LEGION_TEST_OMP=<omp> LEGION_TEST_NATS_URL=nats://127.0.0.1:<port> bun test ./extensions/*-omp.test.ts`.

- `scripts/smoke-*.sh` are manual smokes against a live listener, never CI steps.
  They run tmux on a private socket (`tmux -S "$tmux_socket"`) so cleanup ends only their own session; keep new tmux calls on it.

## Rules

- Build every tool schema field through the injected `pi.zod`: Oh My Pi's converter reads internals only its own Zod produces, and one field from another Zod fails the whole extension load.

- Session state converges in one serialized pass (`reconcile`); its queues, deadlines and registration and role state live in `src/registration-state.ts`, not in the entry.
  A `/new` or `/resume` can land in any await, so after each await a pass, and any branch that spends a person's delivery attempt, reads `sessionOpen` again.

- Role lookups and role records are read by `roleHeldBy` and `mayActOnRoleRecord` (`@legion/envoy-client/transport`), shared with claude-envoy.

- At load the entry sets `DISPATCH_HOST=omp` and `ENVOY_CONFIG_FILE`, and puts `bin/` on `PATH` when no entry resolves to it; `bindDispatchSession` sets `DISPATCH_SESSION_ID`.
  A session with no id sets that to the empty string rather than deleting it: a deleted variable stays in the C environment a child Oh My Pi starts from.

- Do not alter `~/.omp` from this package; the README has the development install.

## Consumer delivery

- A pass subscribes the relay subject, `notifications.envoy.deliver.<session id>`, before any registration; it is the one subject this plugin receives on.

- `renderInbound` (`@legion/envoy-client/delivery`) is the only inbound renderer, and the advertised delivery modes come from `DELIVERY_CAPABILITIES` in `@legion/contracts`, `btw` only on a host with a side turn (`ctx.runEphemeralTurn`).

- Deliver every card as an aside with `triggerTurn`, never a steer: a steer waits in the queue that holds a person's Send.
  The exceptions and the wake a folded card gets are in `src/card-wake.ts` and `src/unwritten-frames.ts`; `extensions/card-delivery-omp.test.ts` proves them on Oh My Pi.

## Each frame shown once

- Answer a frame `done` and record it in the delivery ledger once the host wrote it (the `message_end` of its card or turn), never sooner, and answer `nak` when its claim is released unwritten.
  Drop a frame the ledger already holds: a restart at the consumer's ack floor sends it again.

## Subagents

- A `task` subagent shares its parent's identity (`subagentSessionCheck` in `@legion/pi-shared/subagent-session`): the entry skips its `session_start` and switch events, so it registers nothing and opens no relay subject.

- Its reply address is its parent's, found through the sessions each top-level instance publishes under `Symbol.for("legion.pi-envoy.envoy-session")` (`src/envoy-session.ts`).
  When none matches it has no reply address; never name an unrelated live session.

- The listener forwards a role message to no holder that is its own source, so a subagent's `envoy_publish` to a role its parent holds goes to no session; it reaches the parent by a `write` to the parent's `agent://` address.

## A person's message from Dispatch

- A person's Send or Aside becomes the user's own turn only through `acceptUserTurn` (`@legion/pi-shared/accept-user-turn`), which claude-envoy runs too.
  Only Dispatch's 200 to the accept makes a turn, and the turn runs the stored body it returns, never the frame's text; every other frame is a card.

- The record of handled attempts keys on the attempt a frame names, so a caller who reads `message.created` and names the attempt first spends it: the record's limit, which `acceptUserTurn` explains.

- The live view tags the person's message with its Dispatch id from the injected-user-turn record (`matchInjectedUserTurn`), and the page shows an untagged turn twice.
  Untagged are a turn the record misses (on a host without `sendUserInput`, a Send the host runs as a turn of its own after the run's `agent_end`) and a prompt template or file command, whose stream carries the expanded text.

## Run-end self-check

- A run a person's input armed ends with one side turn that answers WAITING or PROCEEDING, and only a reply whose first word is WAITING steers the agent to open a question in Dispatch.
  The parse is strict because a false WAITING has an agent page a person for nothing, while a false PROCEEDING is only silence.
  A Legion-driven session, a `task` subagent and a run with no UI context are never checked.

## AskGate

- `extensions/askgate/` reviews each Dispatch write of a main agent before it posts, and the `dispatch` CLI enforces it through single-use markers (`@legion/envoy-client/askgate-marker`).
  It refuses no call that holds no Dispatch write, and a gate that fails breaks no valid write.
  It is no sandbox, since a process running as the same user defeats it: keep every check it has, and add none that only such a process would need.

## Tests

- `extensions/envoy.test.ts`'s `beforeEach` points `ENVOY_URL` at an unroutable host, stubs `fetch`, and points `ENVOY_CONFIG_FILE` at an envoy.json with no section and AskGate off.
