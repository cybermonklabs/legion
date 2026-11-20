# Claude Envoy

`@sjawhar/claude-envoy` is the Claude Code plugin for Envoy and Dispatch.
One MCP stdio server (`src/envoy-channel-server.ts`) is the session's Envoy ingress and serves the `envoy_*` tools; the mod (`hooks/mod.ts`) writes into a terminal session; `bin/dispatch` puts the `dispatch` command on the Bash tool's `PATH`.
In a pane `legion claude-bridge` launched, the server also runs the Legion phase worker (`src/legion-*.ts`).
`codex/` and `src/codex/` are the Codex CLI plugin, `@sjawhar/codex-envoy`, built from this source.
`README.md` has the architecture, the bundle and release, and the smokes.

## Commands

- From this directory: `bun run test` (one file: `bun test --timeout=60000 tests/mod.test.ts`), `bun run typecheck` and `bun run lint`.

- `bun scripts/build.ts --out "$(mktemp -d)"` builds a runnable copy outside the repository, and `--codex-out <dir>` the Codex plugin; `bun run verify-build "$(mktemp -d)/built.tar"` builds twice and compares, as the release does.

- The real-binary tests skip unless `LEGION_TEST_CLAUDE` names a Claude Code binary (README, "Local checks").
  `tests/envoy-channel-stall.test.ts` runs only with `LEGION_E2E_ENVOY_STACK=1` and `go` on `PATH`.

## Build and release

- Nothing commits `dist/` or `legion/`, and nothing run locally writes them.
  The `claude_envoy_build` job of `.github/workflows/release.yaml` builds and publishes after a merge that touches a path of `$CLAUDE_ENVOY_VERSION_ARGS`.

- The plugin cache has no `node_modules`, so the bundles inline every dependency, `@legion/contracts` and `@legion/envoy-client` among them.

- `scripts/build-inputs.txt` and `$CLAUDE_ENVOY_VERSION_ARGS` list the same paths.
  A change that makes `scripts/build.ts` or `scripts/verify-build.sh` read a new file adds it to both.

- The build refuses a Bun other than `.bun-version`'s; call the pinned binary by path when a version manager's default differs.

- `.omp-plugin/plugin.json`'s empty `mcpServers` keeps Oh My Pi from starting the server, and `.mcp.json`'s `env` passes `CLAUDE_PROJECT_DIR` alone, since Claude Code substitutes an unset `${VAR}` literally.

## Rules

- `renderInbound` (`@legion/envoy-client/delivery`) is the only inbound renderer, and no receipt is published: a reply inbox on a frame belongs to the JetStream acknowledgement.

- Subscribe the relay subject (`notifications.envoy.deliver.<session_id>`) before any registration under that id, and read the id from the shared `SessionIdentity` at each use: a `/clear` moves the session to a new id.

- Hand the relay the nats.js `Msg` itself: its `data`, `headers` and `reply` are prototype getters that an object spread drops.

- Every registry write goes through the session's one `serialized` chain, and nothing in it waits without a bound, since shutdown's deregistration queues behind it.

- Register the capabilities `sessionCapabilities` computes, never a constant.
  The role record rules are `roleHeldBy` and `mayActOnRoleRecord` in `@legion/envoy-client/transport`, which pi-envoy shares.

- No Dispatch call goes through the MCP server, and no permission relay is carried: no reply path here authenticates its sender.

## The mod

- Every message into a terminal session goes through the mod's `session.message`, and `hooks/mod-writer.ts` decides whether it becomes a prompt or a row: only the mod sees the turn's events and this plugin's writes in one order.

- Answer a frame `done` only on the write the mod reports, a row appended or a prompt the engine took; the writes are idempotent by Envoy event id (`writeOnce`).

- An event that carries `agentId` is a subagent's, never the session's turn (`ofSubagent`).

- `resolveModSocketPath` puts the socket under `XDG_RUNTIME_DIR` or Claude Code's configuration directory, never `TMPDIR`, within a 100-byte path.

- The mod needs Claude Code 2.1.310 or newer; `--bare`, `--safe-mode` and settings that turn hooks or mods off leave a session with no messages.

## Legion panes

- The phase reminder's machine is `@legion/pi-shared`'s; `src/legion-phase-hook.ts` only drives it from Claude Code's hooks.

- Its hook entries are generated into `legion/hooks/hooks.json` (`LEGION_HOOKS` in `scripts/build.ts`), never `hooks/hooks.json`: a plain install has no `legion` server, so each would fail on every turn.
  Only `tests/legion-phase-hook-claude.test.ts`, on the real binary, proves Claude Code arms them.
