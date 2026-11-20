# Pi Shared

`@legion/pi-shared` is a private workspace package, never published, that both Oh My Pi plugins compile in: `@sjawhar/pi-envoy` and `@sjawhar/pi-legion`.
It holds the in-process interface between them, the host types, and the Legion modules that pi-legion and claude-envoy's MCP server both run: the daemon client, the claim boot, the `legion` tool's phase ops and the phase reminder.
Each plugin's `bun build` inlines it; only `@oh-my-pi/*` stays external.

## Commands

From this directory: `bun test` (one file: `bun test src/interface.test.ts`), `bunx tsc --noEmit`, `bunx biome check src/ test/`.

## Rules

- Code both plugins need goes here.
  Neither plugin's shipped code imports the other's; a test may load the other plugin's entry by path (`test/cross-imports.ts` is the check).

- The Legion modules here read no host type, so each harness stays a thin adapter over them.

## The interface (`src/interface.ts`)

- Each plugin bundles its own copy of this module, so the shared object lives on `globalThis` under a `Symbol.for` key.
  Resolve it with `envoyPluginInterface()` at each use and never cache it at module scope; `resetEnvoyPluginInterfaceForTests()` is the one test seam.

- The Envoy entry publishes into it at factory time.
  The Legion entry reads it at `session_start` and refuses to run when it is absent or at another version.

- Any change to the shape of `EnvoyPluginInterface`, or to what either side reads from it, bumps `ENVOY_PLUGIN_INTERFACE_VERSION`.
  Both plugins release from one commit, and the daemon's boot gate refuses a pair whose versions differ.
  `packages/daemon/internal/daemon/promptreferences_omp_test.go` spells the number: change it in the same commit.

- The daemon's load probe (`packages/daemon/internal/daemon/probe.mjs`) reads `ENVOY_PLUGIN_INTERFACE_KEY`, `LEGION_PLUGIN_LOADED_KEY` and `LEGACY_LEGION_LOADED_KEY` by their `Symbol.for` strings, so renaming one is a daemon change too.

- The fake probes in the daemon's Go tests print version 1 for both plugins on purpose: the gate compares the two numbers with each other, not with a release.

- Store only process-wide state on the interface, never a function bound to one extension instance.
  Oh My Pi re-binds every extension factory for each in-process `task` subagent, so a stored callback would be the last subagent's.
  `roleClaim.regained` is the one listener slot, and only a path that establishes a Legion identity sets it.

## Test harness (`test/`)

- `test/omp-harness.ts` runs the real Oh My Pi binary that `LEGION_TEST_OMP` names, for each plugin's `*-omp.test.ts`.
  `spawnRpc` takes absolute extension paths, since `import.meta.dir` here is not the caller's.
  `spawnInteractive` drives the TUI under a pseudo-terminal, and `runLegionPane` runs one Legion pane against stand-ins for the daemon, the Envoy listener and the model gateway.

- `test/host-registry.ts` is the `AgentRegistry` stand-in every plugin's mock host spreads in.

- `test/skills-guard.ts` holds the checks each plugin runs over its staged skills.
