# Pi Envoy

`@sjawhar/pi-envoy` is the Oh My Pi plugin every session loads:
the Envoy tools, delivery of the session's messages from the Envoy listener, the `dispatch` command, the context it adds to model requests, and AskGate.
A Legion pane also loads `@sjawhar/pi-legion`, which reaches this plugin only through the interface in `@legion/pi-shared` (`packages/pi-shared/AGENTS.md`).
`README.md` describes how each part behaves and has the commands and the development install.

## Components

- `extensions/envoy.ts`: the plugin's one entry.

- `src/registration-state.ts`: the session's queues, deadlines, and registration and role state.

- `src/envoy-session.ts`: the sessions each top-level instance publishes, through which a `task` subagent finds its parent's reply address.

- `src/card-wake.ts` and `src/unwritten-frames.ts`: which cards get a turn of their own, and the wake a folded card gets.

- `extensions/askgate/`: AskGate, which reviews each Dispatch write of a main agent before it posts.

## Rules

- Build every tool schema field through the injected `pi.zod`:
  Oh My Pi's converter reads internals only its own Zod produces, and one field from another Zod fails the whole extension load.

- Registration and role state change only through the two serial queues of `src/registration-state.ts`, the reconcile pass (`passes`) and role moves (`roleMoves`), and after each await a pass, and any branch that spends a person's delivery attempt, reads `sessionOpen` again: the session can change at any await.

- Read role lookups and role records through `roleHeldBy` and `mayActOnRoleRecord` (`@legion/envoy-client/transport`), shared with claude-envoy.

- Render inbound messages only through `renderInbound` (`@legion/envoy-client/delivery`).

- Deliver a card as an aside with `triggerTurn`, never a steer, except an inert card, a startup-phase card and a `low` card, which starts no turn (`src/unwritten-frames.ts`, `src/card-wake.ts`): a steer waits in the queue that holds a person's Send.

- A person's Send or Aside becomes the user's own turn only through `acceptUserTurn` (`@legion/pi-shared/accept-user-turn`), which claude-envoy runs too.

- Never give a `task` subagent the reply address of an unrelated live session: one whose parent no published session matches has none.

- AskGate is no sandbox, since a process running as the same user defeats it: keep every check it has, and add none that only such a process would need.

- Do not alter `~/.omp` from this package.
