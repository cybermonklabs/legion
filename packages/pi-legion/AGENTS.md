# Pi Legion

`@sjawhar/pi-legion` is the Oh My Pi plugin a Legion pane loads beside `@sjawhar/pi-envoy`;
outside a Legion pane or the operator's controller its entry does nothing.
The daemon client, the claim boot, the `legion` tool's phase ops and the phase reminder live in `@legion/pi-shared` (`packages/pi-shared/AGENTS.md`), shared with claude-envoy.
`README.md` describes the install and the release, and has the commands.

## Components

- `extensions/legion.ts`: the entry, whose Oh My Pi hooks drive the shared phase machine.

- `src/`: the entry's lifecycle modules, among them the claim (`claim-session.ts`), the controller's claim (`controller-session.ts`) and the turn outcomes it reports to the daemon (`turn-outcome.ts`).

- `agents/`: the task agents Legion's prompts dispatch.

## Rules

- Build every tool schema field through the injected `pi.zod` (`toolSchema` in `src/tools.ts`):
  Oh My Pi's converter reads internals only its own Zod produces, and one field from another Zod fails the whole extension load.

- Do not alter `~/.omp` from this package.
