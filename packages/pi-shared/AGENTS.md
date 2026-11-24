# Pi Shared

`@legion/pi-shared` is a private workspace package, never published, that both Oh My Pi plugins compile in: `@sjawhar/pi-envoy` and `@sjawhar/pi-legion`.

## Components

- `src/interface.ts`: the in-process interface between the two plugins.

- The Legion modules that pi-legion and claude-envoy's MCP server both run:
  the daemon client, the claim boot, the `legion` tool's phase ops and the phase reminder.

- `test/`: the harness for each plugin's real Oh My Pi tests, the mock host's agent registry, and the skills guard.

## Rules

- Code both plugins need goes here.
  Neither plugin's shipped code imports the other's (`test/cross-imports.ts` is the check).

- The Legion modules here read no host type, so each harness stays a thin adapter over them.

- Resolve the interface with `envoyPluginInterface()` at each use, and never cache it at module scope: each plugin bundles its own copy of this module.

- Store only process-wide state on the interface, never a function bound to one extension instance:
  Oh My Pi re-binds every extension factory for each in-process `task` subagent.

- Any change to the shape of `EnvoyPluginInterface`, or to what either side reads from it, bumps `ENVOY_PLUGIN_INTERFACE_VERSION`, and `packages/daemon/internal/daemon/promptreferences_omp_test.go`, which spells the number, changes in the same commit.

- Renaming `ENVOY_PLUGIN_INTERFACE_KEY`, `LEGION_PLUGIN_LOADED_KEY` or `LEGACY_LEGION_LOADED_KEY` is a daemon change too:
  the daemon's load probe (`packages/daemon/internal/daemon/probe.mjs`) reads them by their `Symbol.for` strings.
